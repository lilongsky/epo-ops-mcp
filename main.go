// SPDX-License-Identifier: CC-BY-3.0
// Copyright (c) 2026 lilongsky

package main

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	epo "github.com/patent-dev/epo-ops"
)

// ─── MCP JSON-RPC types ───────────────────────────────────────────────────────

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type Response struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ─── Search tool types ────────────────────────────────────────────────────────

type SearchArgs struct {
	Keywords      string `json:"keywords"`
	MaxResults    int    `json:"max_results"`
	StartPosition int    `json:"start_position"`
	PubDateBefore string `json:"pub_date_before"`
	OutputFile    string `json:"output_file"`
	WorkspaceRoot string `json:"workspace_root"`
}

type SearchResultItem struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Abstract        string `json:"abstract"`
	Rank            int    `json:"rank"`
	PublicationDate string `json:"publication_date,omitempty"`
}

type SearchRequestMetadata struct {
	OriginalKeywords string `json:"original_keywords"`
	NormalizedCQL    string `json:"normalized_cql"`
	Cutoff           string `json:"cutoff"`
	MaxResults       int    `json:"max_results"`
	StartPosition    int    `json:"start_position,omitempty"`
	OutputPath       string `json:"output_path"`
}

type SearchOutput struct {
	EvidenceSchemaVersion int                   `json:"evidence_schema_version"`
	QueryID               string                `json:"query_id"`
	Request               SearchRequestMetadata `json:"request"`
	QuerySHA256           string                `json:"query_sha256"`
	RequestSHA256         string                `json:"request_sha256"`
	Results               []SearchResultItem    `json:"results"`
	TotalResults          int                   `json:"total_results"`
	Range                 string                `json:"range"`
	Status                string                `json:"status,omitempty"`
	FailureClass          string                `json:"failure_class,omitempty"`
	Error                 string                `json:"error,omitempty"`
	ProviderCallsUsed     int                   `json:"provider_calls_used,omitempty"`
	ContinuationError     string                `json:"continuation_error,omitempty"`
	RetrievedThrough      *int                  `json:"retrieved_through,omitempty"`
	UnavailablePositions  []int                 `json:"unavailable_positions,omitempty"`
	// ProviderResponse retains a recognized no-results fault body for
	// diagnostics while keeping the typed SearchOutput result shape successful.
	ProviderResponse string `json:"provider_response,omitempty"`
}

type SearchSummary struct {
	TotalResults         int                `json:"total_results"`
	SavedTo              string             `json:"saved_to"`
	Preview              []SearchResultItem `json:"preview"`
	Status               string             `json:"status,omitempty"`
	ContinuationError    string             `json:"continuation_error,omitempty"`
	ProviderCallsUsed    int                `json:"provider_calls_used,omitempty"`
	RetrievedThrough     *int               `json:"retrieved_through,omitempty"`
	UnavailablePositions []int              `json:"unavailable_positions,omitempty"`
}

const (
	searchOutputEvidenceSchemaVersion = 3
	defaultSearchMaxResults           = 100
	maxSearchMaxResults               = 160
	searchFirstRangeMax               = 100
	searchContinuationMax             = 60
)

func framedSHA256(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		encoded := []byte(part)
		_, _ = hash.Write([]byte(strconv.Itoa(len(encoded))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write(encoded)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func buildSearchMetadata(args SearchArgs, normalizedQuery, outputFile string) (SearchRequestMetadata, string, string, string) {
	request := SearchRequestMetadata{
		OriginalKeywords: args.Keywords,
		NormalizedCQL:    normalizedQuery,
		Cutoff:           args.PubDateBefore,
		MaxResults:       args.MaxResults,
		StartPosition:    args.StartPosition,
		OutputPath:       outputFile,
	}
	querySHA256 := framedSHA256("epo-search-query-v1", request.NormalizedCQL)
	requestSHA256 := framedSHA256(
		"epo-search-request-v1",
		request.OriginalKeywords,
		request.NormalizedCQL,
		request.Cutoff,
		strconv.Itoa(request.MaxResults),
		request.OutputPath,
	)
	queryID := querySHA256
	if request.StartPosition > 1 {
		queryID = fmt.Sprintf("%s:%d", queryID, request.StartPosition)
	}
	return request, querySHA256, requestSHA256, queryID
}

func classifySearchFailure(err error) string {
	if err == nil {
		return "provider_failure"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "epo http") || strings.Contains(message, "status code") || strings.Contains(message, "rate limit") {
		return "service_failure"
	}
	if strings.Contains(message, "timeout") || strings.Contains(message, "deadline") || strings.Contains(message, "connection") || strings.Contains(message, "network") {
		return "transport_failure"
	}
	return "provider_failure"
}

func publicationQuery(keywords, cutoff string) (string, error) {
	if cutoff == "" {
		return keywords, nil
	}
	date, err := time.Parse("20060102", cutoff)
	if err != nil || date.Format("20060102") != cutoff {
		return "", fmt.Errorf("pub_date_before must be a real YYYYMMDD date")
	}
	upper := date.AddDate(0, 0, -1)
	if upper.Format("20060102") < "19000101" {
		return "", fmt.Errorf("pub_date_before must be after 19000101")
	}
	// Keep the date restriction outside the caller's boolean expression so the
	// request explicitly scopes the cutoff to the entire keyword expression.
	return fmt.Sprintf(`(%s) AND pd within "19000101 %s"`, strings.TrimSpace(keywords), upper.Format("20060102")), nil
}

type workspaceOutputHandle struct {
	workspaceRoot       *os.Root
	parentRoot          *os.Root
	configuredRootPath  string
	rootPath            string
	requestedParentPath string
	parentPath          string
	outputPath          string
	outputName          string
	rootInfo            os.FileInfo
	parentInfo          os.FileInfo
}

func (h *workspaceOutputHandle) Close() error {
	var firstErr error
	if h.parentRoot != nil {
		if err := h.parentRoot.Close(); err != nil {
			firstErr = err
		}
	}
	if h.workspaceRoot != nil {
		if err := h.workspaceRoot.Close(); firstErr == nil && err != nil {
			firstErr = err
		}
	}
	return firstErr
}

func validateWorkspaceRoot(workspaceRoot string) (string, string, error) {
	if !filepath.IsAbs(workspaceRoot) {
		return "", "", fmt.Errorf("workspace_root must be an absolute path")
	}
	caller := strings.TrimSpace(os.Getenv("EPO_WORKSPACE_ROOT"))
	if caller == "" || !filepath.IsAbs(caller) {
		return "", "", fmt.Errorf("EPO_WORKSPACE_ROOT must be set to an absolute directory")
	}
	configuredCaller, err := filepath.Abs(caller)
	if err != nil {
		return "", "", fmt.Errorf("invalid EPO_WORKSPACE_ROOT: %w", err)
	}
	callerPath, err := filepath.EvalSymlinks(configuredCaller)
	if err != nil {
		return "", "", fmt.Errorf("resolve EPO_WORKSPACE_ROOT: %w", err)
	}
	callerInfo, err := os.Stat(callerPath)
	if err != nil || !callerInfo.IsDir() {
		return "", "", fmt.Errorf("EPO_WORKSPACE_ROOT must be a directory")
	}
	home, _ := os.UserHomeDir()
	if callerPath == filepath.Dir(callerPath) || (home != "" && callerPath == home) {
		return "", "", fmt.Errorf("EPO_WORKSPACE_ROOT must not be / or the user home")
	}
	configuredRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", "", fmt.Errorf("invalid workspace_root: %w", err)
	}
	rootPath, err := filepath.EvalSymlinks(configuredRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace_root: %w", err)
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil || !rootInfo.IsDir() {
		return "", "", fmt.Errorf("workspace_root must be an existing directory")
	}
	if rootPath == filepath.Dir(rootPath) || (home != "" && rootPath == home) {
		return "", "", fmt.Errorf("workspace_root must not be / or the user home")
	}
	relative, err := filepath.Rel(callerPath, rootPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("workspace_root must stay under EPO_WORKSPACE_ROOT")
	}
	return configuredRoot, rootPath, nil
}

func resolveWorkspaceOutputForRoot(workspaceRoot, outputFile string) (string, string, string, string, error) {
	configuredRootPath, rootPath, err := validateWorkspaceRoot(workspaceRoot)
	if err != nil {
		return "", "", "", "", err
	}
	return resolveWorkspaceOutputFromRoot(configuredRootPath, rootPath, outputFile)
}

func resolveWorkspaceOutput(outputFile string) (string, string, string, string, error) {
	root := strings.TrimSpace(os.Getenv("EPO_WORKSPACE_ROOT"))
	if root == "" {
		return "", "", "", "", fmt.Errorf("EPO_WORKSPACE_ROOT must be set before writing output_file")
	}
	return resolveWorkspaceOutputForRoot(root, outputFile)
}

func resolveWorkspaceOutputFromRoot(configuredRootPath, rootPath, outputFile string) (string, string, string, string, error) {
	if !filepath.IsAbs(outputFile) {
		return "", "", "", "", fmt.Errorf("output_file must be an absolute path under workspace_root")
	}
	outputPath, err := filepath.Abs(outputFile)
	if err != nil {
		return "", "", "", "", fmt.Errorf("invalid output_file: %w", err)
	}
	requestedParentPath := filepath.Dir(outputPath)
	parentPath, err := filepath.EvalSymlinks(requestedParentPath)
	if err != nil {
		return "", "", "", "", fmt.Errorf("output_file parent must already exist: %w", err)
	}
	parentInfo, err := os.Stat(parentPath)
	if err != nil || !parentInfo.IsDir() {
		return "", "", "", "", fmt.Errorf("output_file parent must be a directory: %w", err)
	}
	outputPath = filepath.Join(parentPath, filepath.Base(outputPath))
	relative, err := filepath.Rel(rootPath, parentPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", "", "", fmt.Errorf("output_file must stay under workspace_root")
	}
	return outputPath, configuredRootPath, rootPath, requestedParentPath, nil
}

func workspaceOutputPath(outputFile string) (string, error) {
	outputPath, _, _, _, err := resolveWorkspaceOutput(outputFile)
	return outputPath, err
}

func openWorkspaceOutput(outputFile string) (*workspaceOutputHandle, error) {
	outputPath, configuredRootPath, rootPath, requestedParentPath, err := resolveWorkspaceOutput(outputFile)
	return openWorkspaceOutputResolved(outputPath, configuredRootPath, rootPath, requestedParentPath, err)
}

func openWorkspaceOutputForRoot(workspaceRoot, outputFile string) (*workspaceOutputHandle, error) {
	outputPath, configuredRootPath, rootPath, requestedParentPath, err := resolveWorkspaceOutputForRoot(workspaceRoot, outputFile)
	return openWorkspaceOutputResolved(outputPath, configuredRootPath, rootPath, requestedParentPath, err)
}

func openWorkspaceOutputResolved(outputPath, configuredRootPath, rootPath, requestedParentPath string, err error) (*workspaceOutputHandle, error) {
	if err != nil {
		return nil, err
	}
	workspaceRoot, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	closeRoot := true
	defer func() {
		if closeRoot {
			_ = workspaceRoot.Close()
		}
	}()
	rootInfo, err := workspaceRoot.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace root: %w", err)
	}
	parentPath := filepath.Dir(outputPath)
	parentRelative, err := filepath.Rel(rootPath, parentPath)
	if err != nil || parentRelative == ".." || strings.HasPrefix(parentRelative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("output_file parent must stay under EPO_WORKSPACE_ROOT")
	}
	parentRoot, err := workspaceRoot.OpenRoot(parentRelative)
	if err != nil {
		return nil, fmt.Errorf("open output parent: %w", err)
	}
	handle := &workspaceOutputHandle{
		workspaceRoot:       workspaceRoot,
		parentRoot:          parentRoot,
		configuredRootPath:  configuredRootPath,
		rootPath:            rootPath,
		requestedParentPath: requestedParentPath,
		parentPath:          parentPath,
		outputPath:          outputPath,
		outputName:          filepath.Base(outputPath),
		rootInfo:            rootInfo,
	}
	handle.parentInfo, err = parentRoot.Stat(".")
	if err != nil {
		_ = parentRoot.Close()
		return nil, fmt.Errorf("snapshot output parent: %w", err)
	}
	if err := handle.verifyDirectories(); err != nil {
		_ = parentRoot.Close()
		return nil, err
	}
	if _, err := parentRoot.Lstat(handle.outputName); err == nil {
		_ = parentRoot.Close()
		return nil, fmt.Errorf("output_file target already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = parentRoot.Close()
		return nil, fmt.Errorf("inspect output_file target: %w", err)
	}
	closeRoot = false
	return handle, nil
}

func (h *workspaceOutputHandle) verifyDirectories() error {
	currentRoot, err := os.Stat(h.configuredRootPath)
	if err != nil || !os.SameFile(currentRoot, h.rootInfo) {
		return fmt.Errorf("workspace root changed during output publication")
	}
	currentParentPath, err := filepath.EvalSymlinks(h.requestedParentPath)
	if err != nil || filepath.Clean(currentParentPath) != filepath.Clean(h.parentPath) {
		return fmt.Errorf("output parent changed during output publication")
	}
	currentParent, err := os.Stat(h.requestedParentPath)
	if err != nil || !os.SameFile(currentParent, h.parentInfo) {
		return fmt.Errorf("output parent changed during output publication")
	}
	heldRoot, err := h.workspaceRoot.Stat(".")
	if err != nil || !os.SameFile(heldRoot, h.rootInfo) {
		return fmt.Errorf("held workspace root identity changed")
	}
	heldParent, err := h.parentRoot.Stat(".")
	if err != nil || !os.SameFile(heldParent, h.parentInfo) {
		return fmt.Errorf("held output parent identity changed")
	}
	return nil
}

func (h *workspaceOutputHandle) createTemporary() (*os.File, string, os.FileInfo, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var random [16]byte
		if _, err := cryptorand.Read(random[:]); err != nil {
			return nil, "", nil, fmt.Errorf("generate temporary output name: %w", err)
		}
		name := ".epo-search-" + hex.EncodeToString(random[:]) + ".json"
		file, err := h.parentRoot.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", nil, fmt.Errorf("create output file: %w", err)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, "", nil, fmt.Errorf("stat temporary output: %w", err)
		}
		return file, name, info, nil
	}
	return nil, "", nil, fmt.Errorf("create output file: temporary name collision")
}

func (h *workspaceOutputHandle) removeOwned(name string, owned os.FileInfo) (bool, error) {
	current, err := h.parentRoot.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !os.SameFile(owned, current) {
		return false, nil
	}
	if err := h.parentRoot.Remove(name); err != nil {
		return false, err
	}
	return true, nil
}

func (h *workspaceOutputHandle) publishTemporary(temporaryName string, temporaryInfo os.FileInfo) error {
	removePublished := func() {
		_, _ = h.removeOwned(h.outputName, temporaryInfo)
	}
	if err := h.verifyDirectories(); err != nil {
		return err
	}
	currentTemporary, err := h.parentRoot.Lstat(temporaryName)
	if err != nil {
		return fmt.Errorf("commit output: inspect temporary file: %w", err)
	}
	if !os.SameFile(temporaryInfo, currentTemporary) {
		return fmt.Errorf("commit output: temporary file identity mismatch")
	}
	if err := h.parentRoot.Link(temporaryName, h.outputName); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("commit output: target already exists: %w", err)
		}
		return fmt.Errorf("commit output without replacement: %w", err)
	}
	finalInfo, err := h.parentRoot.Lstat(h.outputName)
	if err != nil || !os.SameFile(temporaryInfo, finalInfo) {
		removePublished()
		return fmt.Errorf("commit output: published file identity mismatch")
	}
	if err := h.verifyDirectories(); err != nil {
		removePublished()
		return err
	}
	removed, err := h.removeOwned(temporaryName, temporaryInfo)
	if err != nil {
		removePublished()
		return fmt.Errorf("remove temporary output: %w", err)
	}
	if !removed {
		removePublished()
		return fmt.Errorf("commit output: temporary file identity changed")
	}
	finalInfo, err = h.parentRoot.Lstat(h.outputName)
	if err != nil || !os.SameFile(temporaryInfo, finalInfo) {
		removePublished()
		return fmt.Errorf("commit output: published file identity changed")
	}
	return nil
}

func (h *workspaceOutputHandle) publishJSON(output interface{}) error {
	if err := h.verifyDirectories(); err != nil {
		return err
	}
	temporary, temporaryName, temporaryInfo, err := h.createTemporary()
	if err != nil {
		return err
	}
	temporaryRemoved := false
	defer func() {
		if !temporaryRemoved {
			_, _ = h.removeOwned(temporaryName, temporaryInfo)
		}
	}()
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode output: %w", err)
	}
	if _, err := io.WriteString(temporary, redactKnownCredentials(encoded.String())); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := h.publishTemporary(temporaryName, temporaryInfo); err != nil {
		return err
	}
	temporaryRemoved = true
	return nil
}

func (h *workspaceOutputHandle) publish(output SearchOutput) error {
	return h.publishJSON(output)
}

func writeSearchOutput(outputFile string, output SearchOutput) error {
	handle, err := openWorkspaceOutput(outputFile)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.publish(output)
}

type publishedSearchClient struct {
	baseURL       string
	httpClient    *http.Client
	authenticator interface {
		GetToken(context.Context) (string, error)
		ClearToken()
	}
}

func newPublishedSearchClient(key, secret string) *publishedSearchClient {
	baseClient := &http.Client{Timeout: 30 * time.Second}
	return &publishedSearchClient{
		baseURL:       "https://ops.epo.org/3.2/rest-services",
		httpClient:    baseClient,
		authenticator: epo.NewAuthenticator(key, secret, baseClient),
	}
}

func (c *publishedSearchClient) search(ctx context.Context, constituent, query, rangeStr string) (string, error) {
	endpoint, err := url.Parse(c.baseURL + "/published-data/search/" + url.PathEscape(constituent))
	if err != nil {
		return "", fmt.Errorf("failed to construct EPO search URL: %w", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("Range", rangeStr)
	endpoint.RawQuery = params.Encode()
	status, body, err := c.requestWithStatus(ctx, http.MethodGet, endpoint.String(), "application/ops+xml", "", nil)
	if err != nil {
		if status == http.StatusNotFound && isNoResultsSearchFault(body) {
			return body, nil
		}
		return "", fmt.Errorf("EPO search request failed: %w", err)
	}
	return body, nil
}

func isNoResultsSearchFault(body string) bool {
	var fault struct {
		XMLName xml.Name `xml:"fault"`
		Code    string   `xml:"code"`
		Message string   `xml:"message"`
	}
	if err := xml.Unmarshal([]byte(body), &fault); err != nil {
		return false
	}
	return fault.XMLName.Space == "http://ops.epo.org" &&
		strings.TrimSpace(fault.XMLName.Local) == "fault" &&
		strings.TrimSpace(fault.Code) == "SERVER.EntityNotFound" &&
		strings.TrimSpace(fault.Message) == "No results found"
}

func (c *publishedSearchClient) request(ctx context.Context, method, endpoint, accept, contentType string, body []byte) (string, error) {
	_, responseBody, err := c.requestWithStatus(ctx, method, endpoint, accept, contentType, body)
	return responseBody, err
}

func (c *publishedSearchClient) requestWithStatus(ctx context.Context, method, endpoint, accept, contentType string, body []byte) (int, string, error) {
	token, err := c.authenticator.GetToken(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("failed to authenticate EPO request: %w", err)
	}

	var lastErr error
	lastStatus := 0
	lastBody := ""
	refreshed := false
	for attempt := 1; attempt <= 3; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if reqErr != nil {
			return 0, "", fmt.Errorf("failed to create EPO request: %w", reqErr)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, doErr := c.httpClient.Do(req)
		if doErr != nil {
			lastErr = doErr
		} else {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				return 0, "", fmt.Errorf("failed to read EPO response: %w", readErr)
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return resp.StatusCode, string(body), nil
			}
			lastStatus = resp.StatusCode
			lastBody = string(body)
			if !refreshed && (resp.StatusCode == http.StatusUnauthorized ||
				resp.StatusCode == http.StatusBadRequest && strings.Contains(string(body), "invalid_access_token")) {
				c.authenticator.ClearToken()
				token, err = c.authenticator.GetToken(ctx)
				if err != nil {
					return 0, "", fmt.Errorf("failed to refresh EPO token: %w", err)
				}
				refreshed = true
				continue
			}
			lastErr = fmt.Errorf("EPO HTTP %d: %s", resp.StatusCode, redactErrorText(strings.TrimSpace(string(body)), token))
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
				return resp.StatusCode, string(body), lastErr
			}
		}

		if attempt < 3 {
			select {
			case <-ctx.Done():
				return 0, "", ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return lastStatus, lastBody, lastErr
}

// ─── Usage stats tool types ──────────────────────────────────────────────────

type UsageArgs struct {
	TimeRange string `json:"time_range"`
}

// ─── Fulltext tool types ──────────────────────────────────────────────────────

type FulltextArgs struct {
	DocID string `json:"doc_id"`
}

type AbstractArgs struct {
	PublicationNumbers []string `json:"publication_numbers"`
	OutputFile         string   `json:"output_file"`
	WorkspaceRoot      string   `json:"workspace_root"`
}

type abstractRequestEvidence struct {
	Method             string   `json:"method"`
	Path               string   `json:"path"`
	ContentType        string   `json:"content_type"`
	Accept             string   `json:"accept"`
	OutputFile         string   `json:"output_file"`
	PublicationNumbers []string `json:"publication_numbers"`
}

type abstractBatchEvidence struct {
	PublicationNumbers []string `json:"publication_numbers"`
	DocDBNumbers       []string `json:"docdb_numbers"`
	RequestBody        string   `json:"request_body"`
	Response           struct {
		HTTPStatus int    `json:"http_status"`
		Body       string `json:"body"`
	} `json:"response"`
}

type abstractResult struct {
	RequestedPublicationNumber string `json:"requested_publication_number"`
	Status                     string `json:"status"`
	PublicationNumber          string `json:"publication_number,omitempty"`
	Abstract                   string `json:"abstract,omitempty"`
	AbstractLanguage           string `json:"abstract_language,omitempty"`
	Title                      string `json:"title,omitempty"`
	PublicationDate            string `json:"publication_date,omitempty"`
}

type abstractEvidence struct {
	SchemaVersion         string                  `json:"schema_version"`
	EvidenceSchemaVersion int                     `json:"evidence_schema_version"`
	Provider              string                  `json:"provider"`
	Tool                  string                  `json:"tool"`
	RecordedAt            string                  `json:"recorded_at"`
	CallReceipt           string                  `json:"call_receipt"`
	Request               abstractRequestEvidence `json:"request"`
	Batches               []abstractBatchEvidence `json:"batches"`
	Results               []abstractResult        `json:"results"`
}

type parsedAbstract struct {
	PublicationNumber string
	Abstract          string
	Language          string
	Title             string
	Date              string
	DateUnavailable   bool
}

const abstractEvidenceSchemaVersion = 1

// ─── Search XML parsing ───────────────────────────────────────────────────────
// Parse the /search/biblio response which contains publication numbers,
// titles, abstracts, and dates in a single XML document.

const (
	epoExchangeNS = "http://www.epo.org/exchange"
	epoOpsNS      = "http://ops.epo.org"
)

type parsedSearchResult struct {
	PubNumber string
	Title     string
	Abstract  string
	Date      string
}

type parsedSearchPage struct {
	Results            []parsedSearchResult
	UnavailableOffsets []int
	ProviderPositions  int
	Total              int
	RangeBegin         int
	RangeEnd           int
	HasRange           bool
}

// parseSearchBiblioXML extracts search results from the /search/biblio XML response.
// Mirrors the logic of Python's _parse_xml_response.
func parseSearchBiblioXML(xmlData string) ([]parsedSearchResult, int) {
	type docIDData struct {
		idType  string
		country string
		number  string
		kind    string
		date    string
	}

	var results []parsedSearchResult
	totalCount := 0

	dec := xml.NewDecoder(strings.NewReader(xmlData))

	// State machine
	var (
		inExchangeDoc  bool
		inBiblio       bool
		inPubRef       bool
		inDocID        bool
		currentDocID   *docIDData
		docdbDocID     *docIDData // the docdb-formatted ID (what we want)
		inInvTitle     bool
		titleText      strings.Builder
		titleCollected string
		inAbstract     bool
		inAbsPara      bool
		absParaText    strings.Builder
		abstractTexts  []string
		inBiblioSearch bool
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		switch t := tok.(type) {
		case xml.StartElement:
			local := t.Name.Local
			ns := t.Name.Space

			switch {
			// Total count from biblio-search element
			case local == "biblio-search":
				inBiblioSearch = true
				for _, a := range t.Attr {
					if a.Name.Local == "total-result-count" {
						fmt.Sscanf(a.Value, "%d", &totalCount)
					}
				}

			// exchange-document: start of a result
			case local == "exchange-document" && ns == epoExchangeNS:
				inExchangeDoc = true
				inBiblio = false
				inPubRef = false
				inDocID = false
				currentDocID = nil
				docdbDocID = nil
				titleCollected = ""
				abstractTexts = nil

			// bibliographic-data
			case local == "bibliographic-data" && ns == epoExchangeNS && inExchangeDoc:
				inBiblio = true

			// publication-reference
			case local == "publication-reference" && ns == epoExchangeNS && inBiblio:
				inPubRef = true

			// document-id within publication-reference
			case local == "document-id" && ns == epoExchangeNS && inPubRef:
				inDocID = true
				currentDocID = &docIDData{}
				for _, a := range t.Attr {
					if a.Name.Local == "document-id-type" {
						currentDocID.idType = a.Value
					}
				}

			// Sub-elements of document-id
			case local == "country" && inDocID && currentDocID != nil:
				// handled in CharData
			case local == "doc-number" && inDocID && currentDocID != nil:
				// handled in CharData
			case local == "kind" && inDocID && currentDocID != nil:
				// handled in CharData
			case local == "date" && inDocID && currentDocID != nil:
				// handled in CharData

			// invention-title
			case local == "invention-title" && ns == epoExchangeNS && inBiblio && titleCollected == "":
				inInvTitle = true
				titleText.Reset()

			// abstract (sibling of bibliographic-data, direct child of exchange-document)
			case local == "abstract" && ns == epoExchangeNS && inExchangeDoc && !inBiblio && len(abstractTexts) == 0:
				inAbstract = true

			// paragraph within abstract
			case local == "p" && inAbstract:
				inAbsPara = true
				absParaText.Reset()
			}

			// Track current field for CharData
			_ = inBiblioSearch

		case xml.EndElement:
			local := t.Name.Local
			ns := t.Name.Space

			switch {
			case local == "document-id" && ns == epoExchangeNS && inPubRef:
				inDocID = false
				if currentDocID != nil && currentDocID.idType == "docdb" {
					docdbDocID = currentDocID
				}
				currentDocID = nil

			case local == "publication-reference" && ns == epoExchangeNS:
				inPubRef = false

			case local == "bibliographic-data" && ns == epoExchangeNS:
				inBiblio = false

			case local == "invention-title" && ns == epoExchangeNS:
				if inInvTitle {
					titleCollected = strings.TrimSpace(titleText.String())
					inInvTitle = false
				}

			case local == "p" && inAbstract:
				if inAbsPara {
					text := strings.TrimSpace(absParaText.String())
					if text != "" {
						abstractTexts = append(abstractTexts, text)
					}
					inAbsPara = false
				}

			case local == "abstract" && ns == epoExchangeNS:
				inAbstract = false
				inAbsPara = false

			case local == "exchange-document" && ns == epoExchangeNS:
				inExchangeDoc = false
				if docdbDocID != nil && strings.TrimSpace(docdbDocID.country) != "" && strings.TrimSpace(docdbDocID.number) != "" {
					pubNum := normalizePatentID(strings.TrimSpace(docdbDocID.country) + strings.TrimSpace(docdbDocID.number) + strings.TrimSpace(docdbDocID.kind))
					results = append(results, parsedSearchResult{
						PubNumber: pubNum,
						Title:     titleCollected,
						Abstract:  strings.Join(abstractTexts, " "),
						Date:      docdbDocID.date,
					})
				}
			}

		case xml.CharData:
			text := string(t)

			// Collect document-id sub-fields
			if inDocID && currentDocID != nil {
				// We need to know which sub-element we're in.
				// Handled via token peeking is complex; use a simpler field tracker.
			}

			if inInvTitle {
				titleText.WriteString(text)
			}
			if inAbsPara {
				absParaText.WriteString(text)
			}
		}
	}
	return results, totalCount
}

// parseSearchBiblioXMLv2 uses a field-tracking approach to handle CharData correctly.
func parseSearchBiblioXMLFull(xmlData string) (parsedSearchPage, error) {
	var results []parsedSearchResult
	totalCount := 0
	sawBiblioSearch := false
	sawTotalCount := false

	dec := xml.NewDecoder(strings.NewReader(xmlData))

	type docIDData struct {
		idType  string
		country string
		number  string
		kind    string
		date    string
	}

	var (
		depth            int
		exchangeDocDepth int
		biblioDepth      int
		pubRefDepth      int
		docIDDepth       int
		abstractDepth    int
		paraDepth        int

		currentDocID *docIDData
		docdbDocID   *docIDData

		exchangeStatusPresent   bool
		exchangeStatusQualified bool
		exchangeStatus          string
		currentField            string
		unavailableOffsets      []int
		providerPositions       int

		inInvTitle     bool
		titleCollected bool
		titleBuf       strings.Builder

		abstractDone bool
		absParaBuf   strings.Builder
		abstractBufs []string

		inPara            bool
		biblioSearchDepth int
		rangeBegin        int
		rangeEnd          int
		rangeSeen         bool
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return parsedSearchPage{}, fmt.Errorf("invalid EPO search XML: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			local := t.Name.Local
			ns := t.Name.Space

			switch local {
			case "biblio-search":
				sawBiblioSearch = true
				biblioSearchDepth = depth
				for _, a := range t.Attr {
					if a.Name.Local == "total-result-count" {
						parsedTotal, parseErr := strconv.Atoi(strings.TrimSpace(a.Value))
						if parseErr != nil || parsedTotal < 0 {
							return parsedSearchPage{}, fmt.Errorf("invalid EPO search total-result-count %q", a.Value)
						}
						totalCount = parsedTotal
						sawTotalCount = true
					}
				}

			case "range":
				if biblioSearchDepth > 0 && (ns == epoOpsNS || ns == "") {
					if rangeSeen {
						return parsedSearchPage{}, errors.New("EPO biblio-search response contains multiple ranges")
					}
					var beginText, endText string
					for _, a := range t.Attr {
						switch a.Name.Local {
						case "begin":
							beginText = strings.TrimSpace(a.Value)
						case "end":
							endText = strings.TrimSpace(a.Value)
						}
					}
					if beginText == "" || endText == "" {
						return parsedSearchPage{}, errors.New("EPO biblio-search range requires begin and end")
					}
					parsedBegin, beginErr := strconv.Atoi(beginText)
					parsedEnd, endErr := strconv.Atoi(endText)
					if beginErr != nil || endErr != nil || parsedBegin < 1 || parsedEnd < parsedBegin {
						return parsedSearchPage{}, fmt.Errorf("invalid EPO biblio-search range %q-%q", beginText, endText)
					}
					rangeBegin = parsedBegin
					rangeEnd = parsedEnd
					rangeSeen = true
				}

			case "exchange-document":
				if ns == epoExchangeNS {
					exchangeDocDepth = depth
					docdbDocID = nil
					currentDocID = nil
					exchangeStatusPresent = false
					exchangeStatusQualified = false
					exchangeStatus = ""
					for _, a := range t.Attr {
						if a.Name.Local == "status" {
							exchangeStatusPresent = true
							exchangeStatusQualified = a.Name.Space != ""
							exchangeStatus = a.Value
						}
					}
					titleCollected = false
					titleBuf.Reset()
					abstractDone = false
					abstractBufs = nil
				}

			case "bibliographic-data":
				if ns == epoExchangeNS && exchangeDocDepth > 0 {
					biblioDepth = depth
				}

			case "publication-reference":
				if ns == epoExchangeNS && biblioDepth > 0 {
					pubRefDepth = depth
				}

			case "document-id":
				if ns == epoExchangeNS && pubRefDepth > 0 {
					docIDDepth = depth
					currentDocID = &docIDData{}
					for _, a := range t.Attr {
						if a.Name.Local == "document-id-type" {
							currentDocID.idType = a.Value
						}
					}
				}

			case "country", "doc-number", "kind", "date":
				if ns == epoExchangeNS && docIDDepth > 0 && currentDocID != nil {
					currentField = local
				}

			case "invention-title":
				if ns == epoExchangeNS && biblioDepth > 0 && !titleCollected {
					inInvTitle = true
					titleBuf.Reset()
				}

			case "abstract":
				if ns == epoExchangeNS && exchangeDocDepth > 0 && biblioDepth == 0 && !abstractDone {
					abstractDepth = depth
					abstractBufs = nil
				}

			case "p":
				if abstractDepth > 0 && depth == abstractDepth+1 {
					paraDepth = depth
					absParaBuf.Reset()
					inPara = true
				}
			}

		case xml.EndElement:
			local := t.Name.Local
			ns := t.Name.Space

			switch local {
			case "biblio-search":
				if biblioSearchDepth == depth {
					biblioSearchDepth = 0
				}

			case "country", "doc-number", "kind", "date":
				if ns == epoExchangeNS {
					currentField = ""
				}

			case "document-id":
				if ns == epoExchangeNS && docIDDepth == depth {
					if currentDocID != nil && currentDocID.idType == "docdb" {
						docdbDocID = currentDocID
					}
					currentDocID = nil
					docIDDepth = 0
				}

			case "publication-reference":
				if ns == epoExchangeNS {
					pubRefDepth = 0
				}

			case "bibliographic-data":
				if ns == epoExchangeNS {
					biblioDepth = 0
				}

			case "invention-title":
				if ns == epoExchangeNS && inInvTitle {
					inInvTitle = false
					titleCollected = true
				}

			case "p":
				if paraDepth == depth && inPara {
					text := strings.TrimSpace(absParaBuf.String())
					if text != "" {
						abstractBufs = append(abstractBufs, text)
					}
					inPara = false
					paraDepth = 0
				}

			case "abstract":
				if ns == epoExchangeNS && abstractDepth == depth {
					abstractDone = true
					abstractDepth = 0
				}

			case "exchange-document":
				if ns == epoExchangeNS && exchangeDocDepth == depth {
					positionOffset := providerPositions
					providerPositions++
					// Count provider slots independently from usable, deduplicated
					// candidates. An exact OPS invalid-result status is the only
					// recognized unavailable slot; arbitrary missing-ID records are
					// not allowed to prove coverage.
					if exchangeStatusPresent {
						if exchangeStatusQualified || exchangeStatus != "invalid result" {
							return parsedSearchPage{}, fmt.Errorf("unknown EPO exchange-document status %q", exchangeStatus)
						}
						unavailableOffsets = append(unavailableOffsets, positionOffset)
					} else {
						country := ""
						number := ""
						kind := ""
						if docdbDocID != nil {
							country = strings.TrimSpace(docdbDocID.country)
							number = strings.TrimSpace(docdbDocID.number)
							kind = strings.TrimSpace(docdbDocID.kind)
						}
						if country != "" && number != "" {
							pubNum := normalizePatentID(country + number + kind)
							results = append(results, parsedSearchResult{
								PubNumber: pubNum,
								Title:     strings.TrimSpace(titleBuf.String()),
								Abstract:  strings.Join(abstractBufs, " "),
								Date:      docdbDocID.date,
							})
						}
					}
					exchangeDocDepth = 0
				}
			}

			depth--

		case xml.CharData:
			text := string(t)
			if inInvTitle {
				titleBuf.WriteString(text)
			}
			if inPara {
				absParaBuf.WriteString(text)
			}
			if currentField != "" && currentDocID != nil {
				switch currentField {
				case "country":
					currentDocID.country += text
				case "doc-number":
					currentDocID.number += text
				case "kind":
					currentDocID.kind += text
				case "date":
					currentDocID.date += text
				}
			}
		}
	}
	if !sawBiblioSearch {
		return parsedSearchPage{}, errors.New("response is not an EPO biblio-search document")
	}
	if !sawTotalCount {
		return parsedSearchPage{}, errors.New("EPO biblio-search response lacks total-result-count")
	}
	return parsedSearchPage{
		Results:            results,
		UnavailableOffsets: unavailableOffsets,
		ProviderPositions:  providerPositions,
		Total:              totalCount,
		RangeBegin:         rangeBegin,
		RangeEnd:           rangeEnd,
		HasRange:           rangeSeen,
	}, nil
}

func validateSearchPageCoverage(page parsedSearchPage, requestedStart, requestedEnd int, expectedTotal *int) error {
	if requestedStart < 1 || requestedEnd < requestedStart {
		return fmt.Errorf("invalid requested search range %d-%d", requestedStart, requestedEnd)
	}
	if expectedTotal != nil && page.Total != *expectedTotal {
		return fmt.Errorf("provider total-result-count changed from %d to %d", *expectedTotal, page.Total)
	}
	availableEnd := requestedEnd
	if page.Total < availableEnd {
		availableEnd = page.Total
	}
	expectedPositions := 0
	if page.Total >= requestedStart && availableEnd >= requestedStart {
		expectedPositions = availableEnd - requestedStart + 1
	}
	if page.ProviderPositions != expectedPositions {
		return fmt.Errorf("provider returned %d provider positions for requested range %d-%d; want %d", page.ProviderPositions, requestedStart, requestedEnd, expectedPositions)
	}
	accountedPositions := len(page.Results) + len(page.UnavailableOffsets)
	if accountedPositions != page.ProviderPositions {
		return fmt.Errorf("provider returned %d accounted positions for requested range %d-%d; got %d provider positions; want %d", accountedPositions, requestedStart, requestedEnd, page.ProviderPositions, expectedPositions)
	}
	if page.HasRange {
		if page.RangeBegin != requestedStart {
			return fmt.Errorf("provider returned range %d-%d for requested range %d-%d", page.RangeBegin, page.RangeEnd, requestedStart, requestedEnd)
		}
		// OPS may echo the requested end even when total-result-count is
		// smaller, or clamp it to the last available result. Accept both.
		if page.RangeEnd != requestedEnd && page.RangeEnd != availableEnd {
			return fmt.Errorf("provider returned range %d-%d for requested range %d-%d", page.RangeBegin, page.RangeEnd, requestedStart, requestedEnd)
		}
	}
	return nil
}

type abstractDocumentID struct {
	IDType  string `xml:"document-id-type,attr"`
	Country string `xml:"country"`
	Number  string `xml:"doc-number"`
	Kind    string `xml:"kind"`
	Date    string `xml:"date"`
}

type abstractParagraph string

func canonicalAbstractXMLText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func decodeAbstractXMLText(decoder *xml.Decoder, start xml.StartElement) (string, error) {
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch token := token.(type) {
		case xml.CharData:
			text.Write(token)
		case xml.StartElement:
			nested, err := decodeAbstractXMLText(decoder, token)
			if err != nil {
				return "", err
			}
			text.WriteString(nested)
		case xml.EndElement:
			if token.Name == start.Name {
				return canonicalAbstractXMLText(text.String()), nil
			}
		}
	}
}

func (paragraph *abstractParagraph) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	text, err := decodeAbstractXMLText(decoder, start)
	if err != nil {
		return err
	}
	*paragraph = abstractParagraph(text)
	return nil
}

type abstractXMLDocument struct {
	BibliographicData struct {
		PublicationReference struct {
			DocumentIDs []abstractDocumentID `xml:"document-id"`
		} `xml:"publication-reference"`
		Titles []string `xml:"invention-title"`
	} `xml:"bibliographic-data"`
	Abstracts []struct {
		Language   string              `xml:"lang,attr"`
		Paragraphs []abstractParagraph `xml:"p"`
	} `xml:"abstract"`
}

var abstractPublicationRe = regexp.MustCompile(`^([A-Z]{2})([0-9]{1,15})([A-Z][0-9]?)$`)

func validateAbstractPublicationDate(value, cutoff string) error {
	date, err := time.Parse("20060102", value)
	if err != nil || date.Format("20060102") != value {
		return fmt.Errorf("EPO publication date must be a real YYYYMMDD date")
	}
	if cutoff != "" {
		cutoffDate, err := time.Parse("20060102", cutoff)
		if err != nil || cutoffDate.Format("20060102") != cutoff || !date.Before(cutoffDate) {
			return fmt.Errorf("EPO publication date must be strictly before the cutoff")
		}
	}
	return nil
}

func normalizeAbstractPublication(value string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	normalized = strings.NewReplacer(" ", "", "-", "", ".", "").Replace(normalized)
	match := abstractPublicationRe.FindStringSubmatch(normalized)
	if match == nil {
		return "", fmt.Errorf("invalid EPO publication number: %s", value)
	}
	return match[1] + match[2] + match[3], nil
}

func abstractDocDB(publicationNumber string) (string, error) {
	normalized, err := normalizeAbstractPublication(publicationNumber)
	if err != nil {
		return "", err
	}
	match := abstractPublicationRe.FindStringSubmatch(normalized)
	return match[1] + "." + match[2] + "." + match[3], nil
}

func parsePatentAbstractsXML(xmlData string, cutoff ...string) ([]parsedAbstract, error) {
	dateCutoff := ""
	if len(cutoff) > 1 {
		return nil, fmt.Errorf("EPO publication date cutoff is ambiguous")
	}
	if len(cutoff) == 1 {
		dateCutoff = cutoff[0]
	}
	decoder := xml.NewDecoder(strings.NewReader(xmlData))
	var results []parsedAbstract
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid EPO exchange XML: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "exchange-document" {
			continue
		}
		if start.Name.Space != epoExchangeNS {
			return nil, fmt.Errorf("EPO exchange-document has an unexpected namespace")
		}
		var document abstractXMLDocument
		if err := decoder.DecodeElement(&document, &start); err != nil {
			return nil, fmt.Errorf("invalid EPO exchange-document: %w", err)
		}
		var docdbID *abstractDocumentID
		for index := range document.BibliographicData.PublicationReference.DocumentIDs {
			item := &document.BibliographicData.PublicationReference.DocumentIDs[index]
			if item.IDType == "docdb" {
				if docdbID != nil {
					return nil, fmt.Errorf("EPO response has ambiguous docdb publication identifiers")
				}
				docdbID = item
			}
		}
		if docdbID == nil || docdbID.Country == "" || docdbID.Number == "" || docdbID.Kind == "" {
			return nil, fmt.Errorf("EPO response document has no unambiguous docdb publication identifier")
		}
		publicationNumber, err := normalizeAbstractPublication(docdbID.Country + docdbID.Number + docdbID.Kind)
		if err != nil {
			return nil, err
		}
		language, abstractText := "", ""
		for _, item := range document.Abstracts {
			paragraphs := make([]string, 0, len(item.Paragraphs))
			for _, paragraph := range item.Paragraphs {
				if value := string(paragraph); value != "" {
					paragraphs = append(paragraphs, value)
				}
			}
			text := canonicalAbstractXMLText(strings.Join(paragraphs, " "))
			if language == "" || strings.EqualFold(item.Language, "en") {
				language, abstractText = strings.TrimSpace(item.Language), text
			}
			if strings.EqualFold(item.Language, "en") {
				break
			}
		}
		title := ""
		if len(document.BibliographicData.Titles) > 0 {
			title = strings.TrimSpace(document.BibliographicData.Titles[0])
		}
		parsed := parsedAbstract{
			PublicationNumber: publicationNumber,
			Abstract:          abstractText,
			Language:          language,
			Title:             title,
			Date:              strings.TrimSpace(docdbID.Date),
		}
		if err := validateAbstractPublicationDate(parsed.Date, dateCutoff); err != nil {
			if dateCutoff != "" {
				return nil, err
			}
			parsed.DateUnavailable = true
		}
		results = append(results, parsed)
	}
	return results, nil
}

func mergeParsedAbstract(existing, duplicate parsedAbstract) (parsedAbstract, error) {
	if existing.PublicationNumber != duplicate.PublicationNumber {
		return parsedAbstract{}, fmt.Errorf("publications do not match")
	}
	merged := existing
	currentAbstract := strings.TrimSpace(existing.Abstract)
	currentLanguage := strings.TrimSpace(existing.Language)
	incomingAbstract := strings.TrimSpace(duplicate.Abstract)
	incomingLanguage := strings.TrimSpace(duplicate.Language)
	if (currentAbstract == "" && incomingAbstract != "") ||
		(incomingAbstract != "" &&
			strings.EqualFold(incomingLanguage, "en") && !strings.EqualFold(currentLanguage, "en")) {
		merged.Abstract = incomingAbstract
		merged.Language = incomingLanguage
	} else {
		merged.Abstract = currentAbstract
		merged.Language = currentLanguage
	}
	if strings.TrimSpace(merged.Title) == "" {
		merged.Title = strings.TrimSpace(duplicate.Title)
	} else {
		merged.Title = strings.TrimSpace(merged.Title)
	}
	currentDate := strings.TrimSpace(existing.Date)
	incomingDate := strings.TrimSpace(duplicate.Date)
	currentDateValid := !existing.DateUnavailable && currentDate != ""
	incomingDateValid := !duplicate.DateUnavailable && incomingDate != ""
	if currentDateValid && incomingDateValid && currentDate != incomingDate {
		return parsedAbstract{}, fmt.Errorf("publication dates conflict")
	}
	switch {
	case currentDateValid:
		merged.Date, merged.DateUnavailable = currentDate, false
	case incomingDateValid:
		merged.Date, merged.DateUnavailable = incomingDate, false
	default:
		merged.Date = currentDate
		if merged.Date == "" {
			merged.Date = incomingDate
		}
		merged.DateUnavailable = true
	}
	return merged, nil
}

// ─── Patent number helpers ────────────────────────────────────────────────────

var usNormRe = regexp.MustCompile(`(?i)^(US)(\d{10})(A\d|B\d)$`)

func normalizePatentID(docID string) string {
	m := usNormRe.FindStringSubmatch(docID)
	if m == nil {
		return docID
	}
	prefix, number, kind := m[1], m[2], m[3]
	number = number[:4] + "0" + number[4:]
	return strings.ToUpper(prefix) + number + strings.ToUpper(kind)
}

func toDocDB(docID string) (string, error) {
	docID = normalizePatentID(docID)
	if len(docID) < 3 {
		return "", fmt.Errorf("patent number too short: %s", docID)
	}
	country := strings.ToUpper(docID[:2])
	if country == "CN" {
		return "", fmt.Errorf("EPO OPS fulltext does not support CN publications: %s", docID)
	}
	rest := docID[2:]

	var number, kind string
	if len(rest) >= 2 {
		last2 := rest[len(rest)-2:]
		last1 := string(rest[len(rest)-1])
		if isLetter(last2[0]) {
			kind = last2
			number = rest[:len(rest)-2]
		} else if isLetter(last1[0]) {
			kind = last1
			number = rest[:len(rest)-1]
		} else {
			number = rest
		}
	} else {
		number = rest
	}

	if kind == "" {
		return country + "." + number, nil
	}
	return country + "." + number + "." + kind, nil
}

func isLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// ─── CQL validation ───────────────────────────────────────────────────────────

var validFieldPrefixes = []string{"ti=", "ab=", "ta=", "pn=", "pd=", "pa=", "in=", "cpc="}

var (
	proxOperatorPattern      = regexp.MustCompile(`(?i)\bprox/(?:distance(?:<=|>=|=|<|>)\d+|unit=(?:word|sentence|paragraph))(?:/ordered=(?:true|false))?\b`)
	fieldAssignmentPattern   = regexp.MustCompile(`(?i)\b(ti|ab|ta)\s*=`)
	rightProxFieldPattern    = regexp.MustCompile(`(?i)^\s*\(?\s*(ti|ab|ta)\s*=`)
	fieldScopedProxShorthand = regexp.MustCompile(`(?i)\b(ti|ab|ta)\s*=\s*\([^)]*\bprox/`)
	bareProxPattern          = regexp.MustCompile(`(?i)\bprox\b`)
)

func validateProximitySyntax(query string) error {
	if fieldScopedProxShorthand.MatchString(query) {
		return fmt.Errorf(
			"CQL prox 语法无效：不要使用 ta=(foo prox/... bar) 形式；请使用 OPS 官方形式 (ta=foo prox/distance<=3 ta=bar)",
		)
	}
	for _, indexes := range bareProxPattern.FindAllStringIndex(query, -1) {
		if indexes[1] >= len(query) || query[indexes[1]] != '/' {
			return fmt.Errorf(
				"CQL prox 语法无效：prox 必须带 distance 或 unit 限定，例如 ta=foo prox/distance<=3 ta=bar",
			)
		}
	}

	operatorIndexes := proxOperatorPattern.FindAllStringIndex(query, -1)
	if strings.Contains(strings.ToLower(query), "prox/") && len(operatorIndexes) == 0 {
		return fmt.Errorf(
			"CQL prox 限定无效：支持 prox/distance<n、prox/distance<=n、prox/unit=word|sentence|paragraph，可选 /ordered=true|false",
		)
	}

	for _, indexes := range operatorIndexes {
		leftFields := fieldAssignmentPattern.FindAllStringSubmatch(query[:indexes[0]], -1)
		rightField := rightProxFieldPattern.FindStringSubmatch(query[indexes[1]:])
		if len(leftFields) == 0 || len(rightField) == 0 {
			return fmt.Errorf(
				"CQL prox 两侧必须显式携带字段，例如 ta=foo prox/distance<=3 ta=bar",
			)
		}
		leftField := strings.ToLower(leftFields[len(leftFields)-1][1])
		rightFieldName := strings.ToLower(rightField[1])
		if leftField != rightFieldName {
			return fmt.Errorf(
				"CQL prox 两侧字段必须相同：检测到 %s 与 %s；请改为同字段形式，例如 ta=foo prox/distance<=3 ta=bar",
				leftField,
				rightFieldName,
			)
		}
	}
	return nil
}

func validateCQL(query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("CQL 检索式不能为空")
	}
	hasField := false
	for _, prefix := range validFieldPrefixes {
		if strings.Contains(query, prefix) {
			hasField = true
			break
		}
	}
	if !hasField {
		return fmt.Errorf("CQL 必须包含字段前缀（ti=、ab=、ta= 等）")
	}
	depth := 0
	for _, ch := range query {
		if ch == '(' {
			depth++
		} else if ch == ')' {
			depth--
		}
		if depth < 0 {
			return fmt.Errorf("CQL 括号不平衡：多余的右括号")
		}
	}
	if depth != 0 {
		return fmt.Errorf("CQL 括号不平衡：缺少 %d 个右括号", depth)
	}
	if err := validateProximitySyntax(query); err != nil {
		return err
	}
	return nil
}

// ─── Tool handlers ────────────────────────────────────────────────────────────

func publishSearchFailureEvidence(
	ctx context.Context,
	outputHandle *workspaceOutputHandle,
	args SearchArgs,
	query string,
	outputFile string,
	windowMetadata bool,
	providerCallsUsed int,
	retrievedThrough int,
	failureErr error,
) error {
	if outputHandle == nil {
		return nil
	}
	normalizedQuery := strings.TrimSpace(query)
	request, querySHA256, requestSHA256, queryID := buildSearchMetadata(args, normalizedQuery, outputFile)
	failure := SearchOutput{
		EvidenceSchemaVersion: searchOutputEvidenceSchemaVersion,
		QueryID:               queryID,
		Request:               request,
		QuerySHA256:           querySHA256,
		RequestSHA256:         requestSHA256,
		Results:               []SearchResultItem{},
		TotalResults:          0,
		Range:                 "1-0",
		Status:                "failed",
		FailureClass:          classifySearchFailure(failureErr),
		Error:                 redactErrorText(failureErr.Error()),
	}
	if windowMetadata {
		failure.ProviderCallsUsed = providerCallsUsed
		failure.RetrievedThrough = &retrievedThrough
	}
	return publishSearchForRequest(ctx, outputHandle, failure)
}

func handleSearchPatents(ctx context.Context, client *publishedSearchClient, raw json.RawMessage) (string, error) {
	var args SearchArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w; search_patents requires a JSON object with keywords (CQL), max_results, pub_date_before, workspace_root and output_file for evidence-backed searches; no EPO request was made", err)
	}
	if args.MaxResults <= 0 {
		args.MaxResults = defaultSearchMaxResults
	}
	if args.MaxResults > maxSearchMaxResults {
		args.MaxResults = maxSearchMaxResults
	}
	if args.StartPosition <= 0 {
		args.StartPosition = 1
	}
	outputFile := ""
	var outputHandle *workspaceOutputHandle
	if args.OutputFile != "" {
		var handle *workspaceOutputHandle
		var err error
		if strings.TrimSpace(args.WorkspaceRoot) != "" {
			handle, err = openWorkspaceOutputForRoot(args.WorkspaceRoot, args.OutputFile)
		} else {
			handle, err = openWorkspaceOutput(args.OutputFile)
		}
		if err != nil {
			return "", err
		}
		outputHandle = handle
		defer outputHandle.Close()
		outputFile = handle.outputPath
	}

	if err := validateCQL(args.Keywords); err != nil {
		return "", err
	}

	query, err := publicationQuery(args.Keywords, args.PubDateBefore)
	if err != nil {
		return "", err
	}

	firstRangeSize := args.MaxResults
	if firstRangeSize > searchFirstRangeMax {
		firstRangeSize = searchFirstRangeMax
	}
	firstRangeEnd := args.StartPosition + firstRangeSize - 1
	rangeStr := fmt.Sprintf("%d-%d", args.StartPosition, firstRangeEnd)

	// Use a direct Published-data request after local validation. The upstream
	// library's simplified CQL tokenizer misclassifies qualified prox operators
	// such as prox/distance<=3 as field names before the request reaches EPO.
	providerCallsUsed := 1
	retrievedThrough := 0
	windowMetadata := args.MaxResults > searchFirstRangeMax || args.StartPosition > 1
	xmlData, err := client.search(ctx, "biblio", query, rangeStr)
	if err != nil {
		// Preserve typed provider/service failure evidence even when the MCP
		// call itself returns an error.  The request
		// metadata and hashes use the same sealed output path as a success.
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", ctx.Err()
		}
		if publishErr := publishSearchFailureEvidence(ctx, outputHandle, args, query, outputFile, windowMetadata, providerCallsUsed, retrievedThrough, err); publishErr != nil {
			return "", fmt.Errorf("search failed: %w; failure evidence write failed: %v", err, publishErr)
		}
		return "", fmt.Errorf("search failed: %w", err)
	}

	noResultsFault := isNoResultsSearchFault(xmlData)
	var firstPage parsedSearchPage
	if noResultsFault {
		// EPO's typed 404 fault is the one valid non-search XML zero-result
		// response.  All other response bodies must pass the biblio-search
		// parser so an HTML/error envelope cannot masquerade as empty results.
		firstPage = parsedSearchPage{}
	} else {
		firstPage, err = parseSearchBiblioXMLFull(xmlData)
		if err != nil {
			parseErr := fmt.Errorf("invalid EPO search response: %w", err)
			if publishErr := publishSearchFailureEvidence(ctx, outputHandle, args, query, outputFile, windowMetadata, providerCallsUsed, retrievedThrough, parseErr); publishErr != nil {
				return "", fmt.Errorf("search failed: %w; failure evidence write failed: %v", parseErr, publishErr)
			}
			return "", fmt.Errorf("search failed: %w", parseErr)
		}
	}
	if coverageErr := validateSearchPageCoverage(firstPage, args.StartPosition, firstRangeEnd, nil); coverageErr != nil {
		parseErr := fmt.Errorf("invalid EPO search response: %w", coverageErr)
		if publishErr := publishSearchFailureEvidence(ctx, outputHandle, args, query, outputFile, windowMetadata, providerCallsUsed, retrievedThrough, parseErr); publishErr != nil {
			return "", fmt.Errorf("search failed: %w; failure evidence write failed: %v", parseErr, publishErr)
		}
		return "", fmt.Errorf("search failed: %w", parseErr)
	}
	// An unavailable slot is position metadata even for a one-range request;
	// expose the provider cursor whenever such metadata is present.
	windowMetadata = windowMetadata || len(firstPage.UnavailableOffsets) > 0
	parsed := firstPage.Results
	totalCount := firstPage.Total

	items := make([]SearchResultItem, 0, len(parsed))
	var unavailablePositions []int
	seenPublicationNumbers := make(map[string]struct{}, len(parsed))
	appendUnavailable := func(offsets []int, pageStart int) {
		for _, offset := range offsets {
			unavailablePositions = append(unavailablePositions, pageStart+offset)
		}
	}
	appendParsed := func(results []parsedSearchResult) {
		for _, r := range results {
			if _, seen := seenPublicationNumbers[r.PubNumber]; seen {
				continue
			}
			seenPublicationNumbers[r.PubNumber] = struct{}{}
			items = append(items, SearchResultItem{
				ID:              r.PubNumber,
				Title:           r.Title,
				Abstract:        r.Abstract,
				Rank:            len(items) + 1,
				PublicationDate: r.Date,
			})
		}
	}
	appendParsed(parsed)
	appendUnavailable(firstPage.UnavailableOffsets, args.StartPosition)
	if windowMetadata && totalCount > 0 {
		retrievedThrough = totalCount
		if retrievedThrough >= args.StartPosition {
			retrievedThrough = firstRangeEnd
			if totalCount < retrievedThrough {
				retrievedThrough = totalCount
			}
		}
	}

	status := ""
	continuationError := ""
	if args.MaxResults > searchFirstRangeMax && totalCount > firstRangeEnd {
		continuationEnd := args.StartPosition + args.MaxResults - 1
		if totalCount < continuationEnd {
			continuationEnd = totalCount
		}
		if continuationEnd > firstRangeEnd+searchContinuationMax {
			continuationEnd = firstRangeEnd + searchContinuationMax
		}
		if continuationEnd > firstRangeEnd {
			providerCallsUsed++
			continuationRange := fmt.Sprintf("%d-%d", firstRangeEnd+1, continuationEnd)
			continuationXML, continuationErr := client.search(ctx, "biblio", query, continuationRange)
			if continuationErr != nil {
				status = "partial"
				continuationError = redactErrorText(continuationErr.Error())
			} else {
				continuationPage, parseErr := parseSearchBiblioXMLFull(continuationXML)
				if parseErr != nil {
					status = "partial"
					continuationError = redactErrorText(parseErr.Error())
				} else if coverageErr := validateSearchPageCoverage(continuationPage, firstRangeEnd+1, continuationEnd, &totalCount); coverageErr != nil {
					status = "partial"
					continuationError = redactErrorText(coverageErr.Error())
				} else {
					appendParsed(continuationPage.Results)
					appendUnavailable(continuationPage.UnavailableOffsets, firstRangeEnd+1)
					retrievedThrough = continuationEnd
				}
			}
		}
	}

	n := len(items)
	normalizedQuery := strings.TrimSpace(query)
	request, querySHA256, requestSHA256, queryID := buildSearchMetadata(args, normalizedQuery, outputFile)
	output := SearchOutput{
		EvidenceSchemaVersion: searchOutputEvidenceSchemaVersion,
		QueryID:               queryID,
		Request:               request,
		QuerySHA256:           querySHA256,
		RequestSHA256:         requestSHA256,
		Results:               items,
		TotalResults:          totalCount,
		Range:                 fmt.Sprintf("1-%d", n),
		Status:                status,
		ContinuationError:     continuationError,
		UnavailablePositions:  unavailablePositions,
	}
	if windowMetadata {
		output.ProviderCallsUsed = providerCallsUsed
		output.RetrievedThrough = &retrievedThrough
	}
	if noResultsFault {
		output.ProviderResponse = redactErrorText(xmlData)
	}

	if outputHandle != nil {
		if err := publishSearchForRequest(ctx, outputHandle, output); err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		preview := items
		if len(preview) > 3 {
			preview = preview[:3]
		}
		summary := SearchSummary{
			TotalResults:         totalCount,
			SavedTo:              outputFile,
			Preview:              preview,
			Status:               status,
			ContinuationError:    continuationError,
			UnavailablePositions: unavailablePositions,
		}
		if windowMetadata {
			summary.ProviderCallsUsed = providerCallsUsed
			summary.RetrievedThrough = &retrievedThrough
		}
		b, _ := json.MarshalIndent(summary, "", "  ")
		return string(b), nil
	}

	b, _ := json.MarshalIndent(output, "", "  ")
	return string(b), nil
}

func handleGetPatentAbstracts(ctx context.Context, client *publishedSearchClient, raw json.RawMessage) (string, error) {
	var args AbstractArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.PublicationNumbers) == 0 {
		return "", fmt.Errorf("publication_numbers must be a non-empty array")
	}
	normalized := make([]string, 0, len(args.PublicationNumbers))
	seen := make(map[string]struct{}, len(args.PublicationNumbers))
	for index, value := range args.PublicationNumbers {
		publicationNumber, err := normalizeAbstractPublication(value)
		if err != nil {
			return "", fmt.Errorf("publication_numbers[%d]: %w", index, err)
		}
		if _, exists := seen[publicationNumber]; exists {
			return "", fmt.Errorf("publication_numbers contains a duplicate: %s", publicationNumber)
		}
		seen[publicationNumber] = struct{}{}
		normalized = append(normalized, publicationNumber)
	}
	var outputHandle *workspaceOutputHandle
	var err error
	if strings.TrimSpace(args.WorkspaceRoot) != "" {
		outputHandle, err = openWorkspaceOutputForRoot(args.WorkspaceRoot, args.OutputFile)
	} else {
		outputHandle, err = openWorkspaceOutput(args.OutputFile)
	}
	if err != nil {
		return "", err
	}
	defer outputHandle.Close()

	evidence := abstractEvidence{
		SchemaVersion:         "epo-patent-abstracts-evidence-v1",
		EvidenceSchemaVersion: abstractEvidenceSchemaVersion,
		Provider:              "epo",
		Tool:                  "get_patent_abstracts",
		RecordedAt:            time.Now().UTC().Format(time.RFC3339Nano),
		Request: abstractRequestEvidence{
			Method:             http.MethodPost,
			Path:               "/published-data/publication/docdb/biblio",
			ContentType:        "text/plain",
			Accept:             "application/exchange+xml",
			OutputFile:         outputHandle.outputPath,
			PublicationNumbers: append([]string(nil), normalized...),
		},
	}
	byPublicationNumber := make(map[string]parsedAbstract, len(normalized))
	for start := 0; start < len(normalized); start += 100 {
		end := start + 100
		if end > len(normalized) {
			end = len(normalized)
		}
		batchNumbers := append([]string(nil), normalized[start:end]...)
		batchRequested := make(map[string]struct{}, len(batchNumbers))
		docdbNumbers := make([]string, 0, len(batchNumbers))
		for _, publicationNumber := range batchNumbers {
			batchRequested[publicationNumber] = struct{}{}
			docdbNumber, err := abstractDocDB(publicationNumber)
			if err != nil {
				return "", err
			}
			docdbNumbers = append(docdbNumbers, docdbNumber)
		}
		requestBody := strings.Join(docdbNumbers, "\n")
		endpoint := strings.TrimRight(client.baseURL, "/") + "/published-data/publication/docdb/biblio"
		status, body, err := client.requestWithStatus(ctx, http.MethodPost, endpoint, "application/exchange+xml", "text/plain", []byte(requestBody))
		if err != nil {
			return "", fmt.Errorf("get_patent_abstracts batch %d failed: %w", len(evidence.Batches)+1, err)
		}
		parsed, err := parsePatentAbstractsXML(body)
		if err != nil {
			return "", fmt.Errorf("get_patent_abstracts batch %d: %w", len(evidence.Batches)+1, err)
		}
		batchByPublicationNumber := make(map[string]parsedAbstract, len(parsed))
		for _, item := range parsed {
			if _, requested := batchRequested[item.PublicationNumber]; !requested {
				return "", fmt.Errorf("EPO response contains an unbound publication for batch %d: %s", len(evidence.Batches)+1, item.PublicationNumber)
			}
			if _, duplicate := byPublicationNumber[item.PublicationNumber]; duplicate {
				return "", fmt.Errorf("EPO response contains a duplicate publication: %s", item.PublicationNumber)
			}
			if existing, duplicate := batchByPublicationNumber[item.PublicationNumber]; duplicate {
				merged, err := mergeParsedAbstract(existing, item)
				if err != nil {
					return "", fmt.Errorf("EPO response duplicate publication identity mismatch for %s: %w", item.PublicationNumber, err)
				}
				batchByPublicationNumber[item.PublicationNumber] = merged
				continue
			}
			batchByPublicationNumber[item.PublicationNumber] = item
		}
		for publicationNumber, item := range batchByPublicationNumber {
			byPublicationNumber[publicationNumber] = item
		}
		batch := abstractBatchEvidence{
			PublicationNumbers: batchNumbers,
			DocDBNumbers:       docdbNumbers,
			RequestBody:        requestBody,
		}
		batch.Response.HTTPStatus = status
		batch.Response.Body = body
		evidence.Batches = append(evidence.Batches, batch)
	}

	evidence.Results = make([]abstractResult, 0, len(normalized))
	for _, requested := range normalized {
		item, found := byPublicationNumber[requested]
		if !found {
			evidence.Results = append(evidence.Results, abstractResult{
				RequestedPublicationNumber: requested,
				Status:                     "publication_not_found",
			})
			continue
		}
		result := abstractResult{
			RequestedPublicationNumber: requested,
			PublicationNumber:          item.PublicationNumber,
		}
		if item.DateUnavailable {
			result.Status = "publication_date_unavailable"
		} else {
			result.Abstract = item.Abstract
			result.AbstractLanguage = item.Language
			result.Title = item.Title
			result.PublicationDate = item.Date
			result.Status = "found"
		}
		if result.Status == "found" && strings.TrimSpace(result.Abstract) == "" {
			result.Status = "abstract_unavailable"
		}
		evidence.Results = append(evidence.Results, result)
	}
	evidence.CallReceipt = framedSHA256("epo-patent-abstracts-evidence-v1", mustJSON(evidence))
	if err := publishForRequest(ctx, func() error { return outputHandle.publishJSON(evidence) }, false); err != nil {
		return "", err
	}
	summary, err := json.Marshal(map[string]interface{}{
		"provider":    "epo",
		"tool":        "get_patent_abstracts",
		"input_count": len(normalized),
		"batch_count": len(evidence.Batches),
		"saved_to":    outputHandle.outputPath,
	})
	if err != nil {
		return "", err
	}
	return string(summary), nil
}

func mustJSON(value interface{}) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func handleGetPatentFulltext(ctx context.Context, client *epo.Client, raw json.RawMessage) (string, error) {
	var args FulltextArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.DocID == "" {
		return "", fmt.Errorf("doc_id is required")
	}

	docdbRef, err := toDocDB(args.DocID)
	if err != nil {
		return "", fmt.Errorf("invalid patent number: %w", err)
	}

	var parts []string
	var claimsErr, descErr error

	claimsData, claimsErr := client.GetClaims(ctx, "publication", "docdb", docdbRef)
	if claimsErr == nil && claimsData != nil {
		var sb strings.Builder
		for _, c := range claimsData.Claims {
			sb.WriteString(fmt.Sprintf("%d. %s\n\n", c.Number, c.Text))
		}
		text := strings.TrimSpace(sb.String())
		if text != "" {
			parts = append(parts, "[Claims]\n"+text)
		}
	}

	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	descData, descErr := client.GetDescription(ctx, "publication", "docdb", docdbRef)
	if descErr == nil && descData != nil {
		var sb strings.Builder
		for _, p := range descData.Paragraphs {
			if p.Text != "" {
				sb.WriteString(p.Text + "\n\n")
			}
		}
		text := strings.TrimSpace(sb.String())
		if text != "" {
			parts = append(parts, "[Description]\n"+text)
		}
	}

	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no fulltext available for %s (claims: %v, description: %v)", args.DocID, claimsErr, descErr)
	}

	result := map[string]string{
		"doc_id":   args.DocID,
		"fulltext": strings.Join(parts, "\n\n"),
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	return string(b), nil
}

func handleGetUsageStats(ctx context.Context, client *epo.Client, raw json.RawMessage) (string, error) {
	var args UsageArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.TimeRange == "" {
		return "", fmt.Errorf("time_range is required (format: dd/mm/yyyy or dd/mm/yyyy~dd/mm/yyyy)")
	}

	stats, err := client.GetUsageStats(ctx, args.TimeRange)
	if err != nil {
		return "", fmt.Errorf("failed to get usage stats: %w", err)
	}

	// Compute totals for convenience
	var totalBytes, totalMessages int64
	for _, e := range stats.Entries {
		totalBytes += e.TotalResponseSize
		totalMessages += int64(e.MessageCount)
	}

	result := map[string]interface{}{
		"time_range":     stats.TimeRange,
		"total_bytes":    totalBytes,
		"total_mb":       fmt.Sprintf("%.2f", float64(totalBytes)/1024/1024),
		"total_messages": totalMessages,
		"entries_count":  len(stats.Entries),
		"entries":        stats.Entries,
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	return string(b), nil
}

func decodeToolArgs(raw json.RawMessage, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// ─── MCP server ───────────────────────────────────────────────────────────────

func toolsListResult() interface{} {
	return map[string]interface{}{
		"tools": []map[string]interface{}{
			{
				"name":        "search_patents",
				"description": "Search patents with EPO OPS Published-data CQL. Use English technical terms for ti/ab/ta indexes and repeat the field on both sides of proximity operators, e.g. ta=green prox/distance<=3 ta=energy.",
				"inputSchema": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"keywords"},
					"properties": map[string]interface{}{
						"keywords": map[string]interface{}{
							"type":        "string",
							"description": "CQL with field prefixes, e.g. ta=battery. For proximity use ta=foo prox/distance<=3 ta=bar.",
						},
						"max_results": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum results (default 100, capped at 160); more than 100 uses a second provider range of at most 60.",
							"default":     100,
						},
						"start_position": map[string]interface{}{
							"type":        "integer",
							"description": "One-based provider start position (default 1). Use retrieved_through + 1 to continue a previous window.",
							"default":     1,
						},
						"pub_date_before": map[string]interface{}{
							"type":        "string",
							"description": "Optional exclusive publication-date cutoff in YYYYMMDD format.",
						},
						"output_file": map[string]interface{}{
							"type":        "string",
							"description": "Optional absolute JSON output path under workspace_root; its parent directory must exist. Returns a summary and up to three preview results.",
						},
						"workspace_root": map[string]interface{}{
							"type":        "string",
							"description": "Optional existing absolute output directory within EPO_WORKSPACE_ROOT; defaults to EPO_WORKSPACE_ROOT.",
						},
					},
				},
			},
			{
				"name":        "get_patent_fulltext",
				"description": "Retrieve available patent claims and description from EPO OPS. Full-text coverage varies by publication.",
				"inputSchema": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"doc_id"},
					"properties": map[string]interface{}{
						"doc_id": map[string]interface{}{
							"type":        "string",
							"description": "Publication number including its country prefix and optional kind code.",
						},
					},
				},
			},
			{
				"name":        "get_patent_abstracts",
				"description": "Retrieve bibliographic data and abstracts by publication number, in batches of at most 100. Save JSON with results in input order.",
				"inputSchema": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"publication_numbers", "output_file"},
					"properties": map[string]interface{}{
						"publication_numbers": map[string]interface{}{
							"type":        "array",
							"minItems":    1,
							"items":       map[string]string{"type": "string"},
							"description": "Unique publication numbers; internally split into batches of at most 100.",
						},
						"output_file": map[string]interface{}{
							"type":        "string",
							"description": "Absolute JSON output path under workspace_root; its parent directory must already exist.",
						},
						"workspace_root": map[string]interface{}{
							"type":        "string",
							"description": "Optional existing absolute output directory within EPO_WORKSPACE_ROOT; defaults to EPO_WORKSPACE_ROOT.",
						},
					},
				},
			},
			{
				"name":        "get_usage_stats",
				"description": "Query EPO OPS API usage statistics for a day or date range.",
				"inputSchema": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"time_range"},
					"properties": map[string]interface{}{
						"time_range": map[string]interface{}{
							"type":        "string",
							"description": "A day (dd/mm/yyyy) or date range (dd/mm/yyyy~dd/mm/yyyy).",
						},
					},
				},
			},
		},
	}
}

func writeResponse(w io.Writer, resp Response) {
	if resp.Error != nil {
		safeError := *resp.Error
		safeError.Message = redactErrorText(safeError.Message)
		resp.Error = &safeError
	}
	if result, ok := resp.Result.(ToolResult); ok {
		result.Content = append([]ToolContent(nil), result.Content...)
		for index := range result.Content {
			if result.IsError {
				result.Content[index].Text = redactErrorText(result.Content[index].Text)
			} else {
				result.Content[index].Text = redactKnownCredentials(result.Content[index].Text)
			}
		}
		resp.Result = result
	}
	b, _ := json.Marshal(resp)
	fmt.Fprintf(w, "%s\n", redactKnownCredentials(string(b)))
}

type executionKey struct{}

type toolExecution struct {
	mu                  sync.Mutex
	cancel              context.CancelFunc
	explicitlyCancelled bool
}

func publishForRequest(ctx context.Context, publish func() error, failure bool) error {
	if execution, ok := ctx.Value(executionKey{}).(*toolExecution); ok {
		execution.mu.Lock()
		defer execution.mu.Unlock()
		if execution.explicitlyCancelled {
			return context.Canceled
		}
	}
	if err := ctx.Err(); err != nil && !(failure && errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	return publish()
}

func publishSearchForRequest(ctx context.Context, handle *workspaceOutputHandle, output SearchOutput) error {
	return publishForRequest(ctx, func() error { return handle.publish(output) }, output.Status == "failed" || output.Status == "partial")
}

// The registered host budget is 60 seconds. Keep authentication, retries and
// all ranges within 55 seconds, leaving time to return a typed failure receipt.
func toolTimeout() time.Duration {
	seconds, err := strconv.ParseFloat(os.Getenv("EPO_TOOL_TIMEOUT_SECONDS"), 64)
	if err != nil || math.IsNaN(seconds) || seconds <= 0 || seconds > 55 {
		seconds = 55
	}
	return time.Duration(seconds * float64(time.Second))
}

func serveMCP(input io.Reader, output io.Writer, budget time.Duration, handle func(context.Context, ToolCallParams) (string, error)) {
	var outputMu, callsMu sync.Mutex
	var pending sync.WaitGroup
	calls := make(map[string]*toolExecution)
	emit := func(response Response) {
		outputMu.Lock()
		defer outputMu.Unlock()
		writeResponse(output, response)
	}
	requestKey := func(id interface{}) string { encoded, _ := json.Marshal(id); return string(encoded) }
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)
	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "notifications/cancelled" {
			var params struct {
				RequestID interface{} `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &params) == nil {
				callsMu.Lock()
				execution := calls[requestKey(params.RequestID)]
				callsMu.Unlock()
				if execution != nil {
					execution.mu.Lock()
					execution.explicitlyCancelled = true
					execution.cancel()
					execution.mu.Unlock()
				}
			}
			continue
		}
		if req.ID == nil {
			continue
		}
		resp := Response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]interface{}{
				"protocolVersion": "2024-11-05", "capabilities": map[string]interface{}{"tools": map[string]interface{}{}},
				"serverInfo": map[string]interface{}{"name": "epo-ops-mcp", "version": "0.1.0"},
			}
		case "ping":
			resp.Result = map[string]interface{}{}
		case "tools/list":
			resp.Result = toolsListResult()
		case "tools/call":
			var params ToolCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				resp.Error = &RPCError{Code: -32602, Message: "invalid params"}
				emit(resp)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			execution := &toolExecution{cancel: cancel}
			ctx = context.WithValue(ctx, executionKey{}, execution)
			key := requestKey(req.ID)
			callsMu.Lock()
			if _, exists := calls[key]; exists {
				callsMu.Unlock()
				cancel()
				resp.Error = &RPCError{Code: -32600, Message: "request id is already active"}
				emit(resp)
				continue
			}
			calls[key] = execution
			callsMu.Unlock()
			pending.Add(1)
			go func(ctx context.Context, params ToolCallParams, resp Response, key string, execution *toolExecution) {
				defer pending.Done()
				defer func() { execution.cancel(); callsMu.Lock(); delete(calls, key); callsMu.Unlock() }()
				text, callErr := handle(ctx, params)
				execution.mu.Lock()
				defer execution.mu.Unlock()
				if execution.explicitlyCancelled || errors.Is(ctx.Err(), context.Canceled) {
					return
				}
				if ctx.Err() != nil {
					callErr = ctx.Err()
				}
				if callErr != nil {
					resp.Result = ToolResult{Content: []ToolContent{{Type: "text", Text: callErr.Error()}}, IsError: true}
				} else {
					resp.Result = ToolResult{Content: []ToolContent{{Type: "text", Text: text}}}
				}
				emit(resp)
			}(ctx, params, resp, key, execution)
			continue
		default:
			resp.Error = &RPCError{Code: -32601, Message: "method not found"}
		}
		emit(resp)
	}
	callsMu.Lock()
	for _, execution := range calls {
		execution.mu.Lock()
		execution.explicitlyCancelled = true
		execution.cancel()
		execution.mu.Unlock()
	}
	callsMu.Unlock()
	pending.Wait()
}

func main() {
	key := os.Getenv("EPO_CLIENT_KEY")
	secret := os.Getenv("EPO_CLIENT_SECRET")
	if key == "" || secret == "" {
		log.Fatal("EPO_CLIENT_KEY and EPO_CLIENT_SECRET must be set")
	}

	client, err := epo.NewClient(&epo.Config{
		ConsumerKey:    key,
		ConsumerSecret: secret,
	})
	if err != nil {
		log.Fatalf("failed to create EPO client: %s", redactErrorText(err.Error()))
	}
	searchClient := newPublishedSearchClient(key, secret)

	serveMCP(os.Stdin, os.Stdout, toolTimeout(), func(ctx context.Context, params ToolCallParams) (string, error) {
		switch params.Name {
		case "search_patents":
			return handleSearchPatents(ctx, searchClient, params.Arguments)
		case "get_patent_fulltext":
			return handleGetPatentFulltext(ctx, client, params.Arguments)
		case "get_patent_abstracts":
			return handleGetPatentAbstracts(ctx, searchClient, params.Arguments)
		case "get_usage_stats":
			return handleGetUsageStats(ctx, client, params.Arguments)
		default:
			return "", fmt.Errorf("unknown tool: %s", params.Name)
		}
	})
}

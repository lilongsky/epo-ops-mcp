# EPO OPS MCP

为 Claude 云端会话提供 EPO Open Patent Services（OPS）访问的独立 stdio MCP 服务器。
服务器名为 `epo`，保留以下四个工具：

| 工具 | 功能 |
| --- | --- |
| `search_patents` | CQL 专利检索、公开日截止限制和分页 |
| `get_patent_abstracts` | 批量获取著录信息及摘要 |
| `get_patent_fulltext` | 获取 OPS 可提供的权利要求和说明书 |
| `get_usage_stats` | 查询 OPS API 用量 |

`get_patent_fulltext` 会在本地直接拒绝 CN 公开文本，调用方需另取其全文。
这项限制不适用于检索和摘要工具。

## 环境要求

- Ubuntu 24.04 x86_64，Go 1.21 或更新版本，Python 3、Bash。
- `curl`、`flock`（util-linux）和 `timeout`（coreutils）。
- 启动器使用 `GOTOOLCHAIN=auto`；预装 Go 低于 1.25.0 时，会按 `go.mod` 自动下载所需的 Go 1.25.0 工具链。
- 首次构建或可选预热时，可以访问下表中的公共下载域名。

| 阶段 | HTTPS 白名单域名 | 用途 |
| --- | --- | --- |
| MCP 运行时 | `ops.epo.org` | OAuth 认证、检索、摘要、全文和用量查询，使用 443 端口 |
| 构建／可选预热 | `raw.githubusercontent.com` | setup 获取公开的 `go.mod`、`go.sum` |
| 构建／可选预热 | `proxy.golang.org`、`sum.golang.org` | Go 工具链、固定依赖的下载与校验 |
| 构建／可选预热 | `storage.googleapis.com` | Go 模块文件下载可能重定向至此 |

`www.epo.org` 等源码中的 XML namespace 不是网络请求地址。
构建／预热的域名与 MCP 运行时白名单分别配置；缓存已就绪时，工具调用只需访问 `ops.epo.org`。

## 凭据

只需在 Claude 网页端配置环境变量名：

- `EPO_CLIENT_KEY`
- `EPO_CLIENT_SECRET`

不要将变量值填入仓库文件。根目录 `.mcp.json` 只引用上述环境变量。
服务器不加载 `.env`、密钥文件或本地配置文件；缺少变量时会报出变量名并退出。

可选 `EPO_TOOL_TIMEOUT_SECONDS` 控制工具调用总超时，默认 55 秒，支持大于 0 且不超过 55 的秒数。
`EPO_WORKSPACE_ROOT` 由启动器设置为仓库外的输出目录，无需在 Claude 中填写。

## 安装和启动

会话克隆本仓库并读取根目录 `.mcp.json` 后，会以相对路径执行 `./launch.sh`。
启动器按 Go 源码和依赖锁定文件的 hash 自动构建，首次编译预计约 20 秒；后续启动复用相同 hash 的二进制。
需要下载工具链或依赖时，启动耗时也取决于下载速度。

setup 仅用于可选预热缓存。云端 setup 可能在克隆仓库之前执行；需要预热时，
将 [setup.sh](setup.sh) 的完整内容粘贴到网页端的 setup script 中。
该脚本会获取公开的 `go.mod`、`go.sum`，提前下载依赖。

本地已经克隆仓库时，也可在仓库根目录运行：

```sh
./setup.sh
```

不执行 setup 也可直接启动，由启动器完成所需下载和编译。
预热脚本始终以退出码 `0` 结束；预热未完成时，启动器仍会在首次启动时完成所需下载和编译。

缓存优先使用以下仓库外目录：

| 目录 | 内容 |
| --- | --- |
| `/opt/epo-go/path` | `GOPATH` |
| `/opt/epo-go/mod` | Go 模块及下载的工具链 |
| `/opt/epo-go/cache` | Go 编译缓存 |
| `/opt/epo-go/bin` | 按源码 hash 缓存的 MCP 二进制 |

`/opt` 不可写时，启动器将以上目录统一切换到 `${TMPDIR:-/tmp}/epo-go` 下的 `path`、`mod`、`cache`、`bin`。
构建文件、缓存、二进制和工具输出均放在仓库外，不向仓库写文件。

也可在仓库根目录手动启动：

```sh
./launch.sh
```

传输为逐行 JSON-RPC over stdio；支持 MCP 初始化、工具列表、调用、ping 和取消请求。
stdout 只输出协议消息。工具参数与必填项通过 `tools/list` 返回。

## 自测

环境变量已配置后：

```sh
python3 smoke_test.py
```

此命令验证初始化、四个工具的发现、错误输入和进程退出，不调用真实 API。
需要验证真实检索时，在进程环境中另外设置 `EPO_TEST_QUERY`，然后运行：

```sh
python3 smoke_test.py --query "$EPO_TEST_QUERY"
```

测试只打印检查摘要，不保存凭据、检索式或查询记录到仓库。
合法零结果、服务错误和分页不完整使用不同状态表示；文件输出不会覆盖已有文件。

## 如何更新本仓库

1. 在 **Settings → Rules → Rulesets** 中，将 **read-only-all-branches** 切换为 **Disabled**。
2. 在本地修改、测试并扫描待提交内容，再通过普通的 `git push origin main` 推送更新。
3. 推送完成后，立即将该规则切回 **Active**，保持没有绕过对象。
4. 执行以下 6.1、6.2 验证；规则配置必须吻合，两个测试推送都必须被拒绝。
5. 清理本地测试分支和空提交，确保本地 `main` 与远端 `origin/main` 一致。

| 验证 | 操作与预期 |
| --- | --- |
| 6.1 配置核验 | 用 `gh api` 读取 `read-only-all-branches` 规则，确认 `enforcement` 为 `active`、`bypass_actors` 为空、分支包含 `~ALL` 且排除列表为空，并有 `creation`、`update`、`deletion`、`non_fast_forward` 四项限制 |
| 6.2 推送验证 | 在本地新建 `ruleset-test` 分支并做空提交，尝试推送；再向本地 `main` 添加空提交并尝试推送；两次均应被拒绝 |

任一测试推送成功时，应立即停止并检查规则配置。

全分支规则限制 GitHub 分支的创建、更新、删除和非快进推送。
Issues、Wiki、Projects、Discussions 关闭，密钥扫描和推送保护启用。
规则不影响公开克隆和读取。

## 许可证

Copyright (c) 2026 lilongsky。
本项目服务代码为独立实现；原创代码采用
[CC BY 4.0 International](https://creativecommons.org/licenses/by/4.0/)，完整文本见 [LICENSE](LICENSE)。

第三方依赖保留各自的 MIT、BSD-3-Clause 或 Apache-2.0 许可，原始许可文本和署名见
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 及 [LICENSES](LICENSES/)。
本项目许可证不重新授权 EPO 返回的数据，数据使用依 EPO 的服务条款。

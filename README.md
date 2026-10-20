# EPO OPS MCP

为 Claude 云端会话提供 EPO Open Patent Services（OPS）访问的独立 stdio MCP 服务器。
服务器名为 `epo`，保留以下四个工具：

| 工具 | 功能 |
| --- | --- |
| `search_patents` | CQL 专利检索、公开日截止限制和分页 |
| `get_patent_abstracts` | 批量获取著录信息及摘要 |
| `get_patent_fulltext` | 获取 OPS 可提供的权利要求和说明书 |
| `get_usage_stats` | 查询 OPS API 用量 |

## 环境要求

- Ubuntu 24.04 x86_64，Go 1.25.0 或更新版本，Python 3、Bash。
- setup 阶段可以访问公共 Go 模块代理。
- 运行时 HTTPS 白名单：`ops.epo.org`（443 端口）。OAuth 认证、检索、摘要、全文和用量查询都使用该主机。

`www.epo.org` 等源码中的 XML namespace 不是网络请求地址。
setup 阶段的模块下载使用 `proxy.golang.org`、`sum.golang.org`，模块文件下载可能重定向至 `storage.googleapis.com`；这些与 MCP 运行时的白名单分开配置。

## 凭据

只需在 Claude 网页端配置环境变量名：

- `EPO_CLIENT_KEY`
- `EPO_CLIENT_SECRET`

不要将变量值填入仓库文件。根目录 `.mcp.json` 只引用上述环境变量。
服务器不加载 `.env`、密钥文件或本地配置文件；缺少变量时会报出变量名并退出。

可选 `EPO_TOOL_TIMEOUT_SECONDS` 控制工具调用总超时，默认 55 秒，支持大于 0 且不超过 55 的秒数。
`EPO_WORKSPACE_ROOT` 由启动器设置为仓库外的输出目录，无需在 Claude 中填写。

## 安装和启动

将会话的 setup script 设置为：

```sh
./setup.sh
```

setup 使用预装 Go，依照 `go.mod` 和 `go.sum` 安装固定依赖并构建二进制。
脚本有总超时限制；成功返回退出码 `0`，失败会保留非零退出码。
构建目录、Go 缓存、二进制和工具输出均位于仓库外的临时运行目录，不向仓库写文件。

会话克隆本仓库并读取根目录 `.mcp.json` 后，会以相对路径执行 `./launch.sh`。
也可在仓库根目录手动启动：

```sh
./launch.sh
```

传输为逐行 JSON-RPC over stdio；支持 MCP 初始化、工具列表、调用、ping 和取消请求。
stdout 只输出协议消息。工具参数与必填项通过 `tools/list` 返回。

## 自测

setup 完成且环境变量已配置后：

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
2. 在本地修改、测试并扫描待提交内容，再推送更新。
3. 推送完成后，将该规则切回 **Active**，保持没有绕过对象。
4. 重新验证分支创建和 main 更新均被拒绝。

全分支规则限制 GitHub 分支的创建、更新、删除和非快进推送。
Issues、Wiki、Projects、Discussions 关闭，密钥扫描和推送保护启用。
规则不影响公开克隆和读取。

## 许可证

Copyright (c) 2026 lilongsky。
本项目服务代码为独立实现；原创代码采用
[CC BY 3.0 Unported](https://creativecommons.org/licenses/by/3.0/)，完整文本见 [LICENSE](LICENSE)。

第三方依赖保留各自的 MIT、BSD-3-Clause 或 Apache-2.0 许可，原始许可文本和署名见
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 及 [LICENSES](LICENSES/)。
本项目许可证不重新授权 EPO 返回的数据，数据使用依 EPO 的服务条款。

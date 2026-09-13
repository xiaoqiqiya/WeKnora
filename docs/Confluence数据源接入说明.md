# Confluence 数据源接入

员工在 Confluence 发布和维护文档，WeKnora 将指定 Space 或父页面子树同步到绑定的文档知识库。首次全量导入，之后默认每 10 分钟增量检查；不回写 Confluence。

## 前置条件

- 部署包含 Confluence 连接器的新版本 WeKnora。原版本只有连接器类型元数据，不能据此判断可用。
- 创建专用文档知识库，并确认存储、向量索引和嵌入模型可用；FAQ 库不支持本流程。
- Confluence 使用专用服务账号，其权限应限定在允许同步的内容范围。
- Server/Data Center 使用个人访问令牌 PAT（Bearer）；Basic 认证填写用户名与 Token，Cloud 填写邮箱与 API Token。
- 填写实际站点地址，包括 `/confluence`、`/wiki` 等上下文路径；连接器在其后拼接 `/rest/api`。
- 内网站点使用 WeKnora 现有 SSRF 白名单设置或 `SSRF_WHITELIST_EXTRA` 放行确切主机。下载重定向涉及其他主机时同样受 SSRF 校验，不应关闭全局防护。
- HTTPS 使用正常受信任的证书；本功能不继承 MCP 的全局 SSL 验证关闭行为。
- 数据源管理、测试连接和同步控制要求 Admin 权限；API Key 还受 `manage_datasources` 与相关路由策略限制。

## 在界面中配置

1. 打开目标知识库 → 设置 → 数据源 → 添加数据源 → Confluence。
2. 填写站点地址和 Token。使用 PAT 时用户名留空；Basic 时填写用户名或邮箱。
3. 测试连接，然后选择 Space，或展开 Space 选择父页面子树。
4. 可设置发布标签，如 `weknora-published`；只同步匹配标签的 current 页面。
5. 选择是否同步附件、图片。默认开启；图片 OCR 需要目标知识库配置视觉模型并启用多模态解析。
6. 保存后自动触发一次同步。默认六段 Cron 为 `0 */10 * * * *`，含秒。
7. 在同步日志和知识列表中检查结果，再用搜索或问答核对正文、附件及图片 OCR。

一个数据源绑定一个知识库。同一知识库避免重复选择范围重叠的数据源。源端页面访问权限不会自动映射到目标用户权限，应按部门或安全等级划分目标知识库。

## 正文与宏

普通正文、表格、列表、链接和代码块转换为 Markdown。优先读取 Confluence `body.export_view`，同步宏的源端渲染结果；普通静态宏可在 storage XHTML 中转换。

动态宏（如 Jira、引用页面、按标签聚合内容）每轮检查源端渲染内容，即使页面版本不变也会核对变化。比较转换后的内容，内容未变不重复入库。

宏同步的是当前账号可见的渲染结果，不是在 WeKnora 执行宏。第三方插件、仅通过浏览器 JavaScript 加载的内容、权限受限或插件故障的宏需用实际页面核验。动态宏未返回 export_view，或返回明确渲染错误时会报错并保留旧副本，不作为成功的完整文档导入。

## 图片与附件

通过 Confluence 页面附件 API 枚举，并使用同一专用账号下载，分别导入相同知识库。每个附件保留父页面 ID、附件 ID、版本和来源页面链接。

- 图片按图片文件进入 WeKnora 多模态解析和 OCR 管道；没有视觉模型时会记录明确失败，后续可重试。
- PDF、Office、文本等附件是否可解析，以部署中启用的解析器和文件格式能力为准；不支持的格式显示在同步日志中。
- 附件版本单独检查；仅修改附件而没有增加父页面版本，仍会同步。
- 下载文件上限为 100 MiB，实际解析还受服务能力和存储配额限制。
- 私有附件通过认证下载后再上传，不直接把带登录要求的 URL 交给通用网页抓取器。
- 云端下载跳转到签名 CDN 地址时不转发 Confluence 凭证或 Cookie；仍保留出站 SSRF 校验。
- 正文保留图片和附件引用说明，文件在知识库中作为关联文档独立保存和检索。外部站点图片、插件生成而未列为页面附件的资源，需要按实际页面另行验证；不能以正文导入成功推断其已 OCR。

## 更新与失败恢复

页面以数据源和 page_id 标识，附件以父 page_id 和 attachment_id 标识，改名不会新建另一条源身份。

更新先导入隐藏的新副本。新副本完整解析完成后，在后续定时或手动同步中启用并替换旧副本；抓取、上传、解析失败不会删除旧的可检索副本。替换后的 WeKnora knowledge_id 会变化。

游标只确认已经完成解析并启用的版本。尚未完成、失败或被取消的版本保持可重试状态。大量文档按流式 Emit/Checkpoint 入库，失败后从已确认页面继续收敛。

更新生效时间包括调度等待、解析耗时，以及安全替换的后续同步。默认调度下可能跨两个或更多周期，不能承诺修改后恰好 10 分钟完成。可以在解析完成后手动同步，立即核对替换。

同步日志成功表示该轮抓取/提交无失败，仍需检查知识的 `parse_status=completed`、`enable_status=enabled` 和实际检索结果。

## 删除行为

自动删除默认关闭且当前连接器不发出删除事件。源端消失、移出范围或撤销标签的页面/附件保留已有副本，管理员需明确处理。403、404 或列表缺失不能直接用于判断删除。

在 WeKnora 删除数据源连接只解除绑定、移除调度并取消待执行同步；已导入文档保留，不会删除 Confluence 原文。仅需临时停止时使用暂停。

源端权限撤回后副本不会自动清除。因此受限内容应进入范围和读者都确定的专用知识库，权限变更时需及时暂停、审查或清理相应副本。

## 真实环境验收

测试工具不包含真实 Key，使用 Node.js 20+，无需新依赖。将 `scripts/confluence-sync-test.example.json` 复制到项目根目录 `.confluence-sync-test.local.json` 并填写；隐藏本地文件已被 `.gitignore` 忽略，不提交凭证。

必填：Confluence 地址、Token、WeKnora 地址、API Key、专用测试知识库 ID、测试页面 ID。正文/附件/图片中选择一段独特的原文填写 `query`，工具要求导入文档的检索结果包含这段文字，避免把无关向量命中误判为成功。默认仅允许只读检查。

```powershell
node scripts/confluence-sync-smoke.mjs .confluence-sync-test.local.json --check
```

确认专用测试库和所选测试页面子树后，设置 `allow_test_writes=true`，执行导入、解析、检索和重复同步检查：

```powershell
node scripts/confluence-sync-smoke.mjs .confluence-sync-test.local.json --sync
```

工具创建的数据源 ID 会输出并写入根目录 `.confluence-sync-test-report.json`。将 ID 填回配置的 `data_source_id` 后重试，不重复创建绑定。报告不包含 Key 或文档正文。

员工修改测试页，或替换同名附件/图片后，再执行 `--sync`，核对源端新版本、解析状态和指定检索词。复杂宏、图片 OCR 与每种附件格式需要分别选样本验证。

确认调度配置活跃后，验证自动同步：

```powershell
node scripts/confluence-sync-smoke.mjs .confluence-sync-test.local.json --watch
```

watch 模式只轮询，不发手动同步请求，最长等待由 `timeout_seconds` 控制。测试期间不要另外手动触发任务，以便判断自动调度。宏、附件或安全替换可能需要更多周期，按测试数据量配置超时。

最后对网络中断、无效凭证、源权限变化、解析模型不可用进行受控测试，确认旧版仍可检索且失败可定位。不得在生产模型或生产账号上直接修改权限来制造故障。

本地检查：

```powershell
go test ./internal/datasource/connector/confluence
go test ./internal/application/service -run TestConfluence
go test ./internal/application/repository -run TestCheckKnowledgeExists
node --test scripts/confluence-sync-smoke.test.mjs
```

### 当前本地验证状态（2026-09-13）

- Confluence 连接器测试、数据源与安全替换相关服务测试、来源身份去重测试通过。
- 前端类型检查、12 项语言键检查、生产构建通过；联调脚本模拟接口测试通过，包含服务端分页限流和无关检索结果拒绝检查。
- 全部数据源扩大回归中，部分已有飞书、语雀域名测试失败：本机 DNS 返回 `198.18.*` 保留地址，触发现有 SSRF 校验；未关闭防护。
- Windows 服务入口 Go 编译检查在临时复用现有 SQLite 头文件后可继续，但容器测试程序链接遇到 DuckDB 静态库与本机 MinGW 的 `__emutls_v` 符号不兼容。完整后端可执行文件构建尚未验收。
- 已获得 Confluence 真实站点配置并通过下述指定页面的认证、抓取、转换和附件下载检查；尚未获得 WeKnora 测试接口与目标知识库配置，真实入库、图片 OCR、检索和自动调度未验收。

### 指定页面的真实读取验证（2026-09-13）

在 WeKnora 中直接运行实际 Confluence 连接器，只读加载 MCP 项目的 `.env`，未修改 MCP 源码或 Confluence 原文。页面 `327753`（ops / 网络穿透配置参考）当前版本为 6；正文与 export_view 获取成功，Markdown 转换成功，11 个代码宏的原始内容均保留。首次抓取输出 1 个页面和 3 个附件，随后未变内容抓取输出 0 条变更。

3 个附件均下载成功，分别为 ZIP、GZIP、GZIP。WeKnora 现有导入格式集合不包含 zip/gz；下载成功不能证明这些压缩包可入库或被检索。正文另有 1 条图片引用提示，附件列表中没有图片，本样本未验证图片 OCR。目标库导入、解析索引、检索和自动调度仍未验收。

以下命令在 WeKnora 根目录运行；测试仅在本进程中允许所选 Confluence 主机，凭证和正文不输出：

```powershell
$env:CONFLUENCE_LIVE_ENV_FILE = 'D:\tdt\python_project\mcp-atlassian\.env'
$env:CONFLUENCE_LIVE_PAGE_ID = '327753'
go test ./internal/datasource/connector/confluence -run '^TestConfluenceLiveRead$' -v -count=1
```

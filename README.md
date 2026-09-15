# CPA Codex Wakeup 插件

[![CI](https://github.com/helloshu/cpa-plugin-codex-wakeup/actions/workflows/ci.yml/badge.svg)](https://github.com/helloshu/cpa-plugin-codex-wakeup/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/helloshu/cpa-plugin-codex-wakeup)](https://github.com/helloshu/cpa-plugin-codex-wakeup/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

这是一个面向 CLIProxyAPI v7 原生插件 ABI 的 Go `c-shared` 动态库。它通过宿主提供的 `host.auth.list`、`host.auth.get` 和 `host.http.do` 回调，按任务周期给选定的 Codex OAuth 账号发送一个很短的 Responses 请求。请求使用短指令 `Reply with exactly OK.`，但这只是提示词偏好，不承诺硬性输出 token 上限。

它不会修改 OAuth 文件、不会刷新或禁用账号、不会保存 access/refresh/id token，也不会把完整上游响应写入状态或日志。它只是产生真实的上游请求，可能触发官方的用量统计或窗口活动；不会延长官方额度上限，也不保证账号一定被“保活”。请自行评估账号风控、额度和服务条款风险。

## 适用基线

当前实现按 CLIProxyAPI `v7.2.151`（commit `5208aec703b5ce7e3445f6e9d91cc13b3e78003a`）的原生插件 ABI 编写：ABI 版本为 1，插件最高 RPC schema 版本为 5。注册和重新配置时会读取宿主请求的 `schema_version`，以 `min(宿主版本, 5)` 应答；宿主缺失或传入 0 按 schema 1 处理，绝不会返回 0。因此也可被只支持 schema 4 或更低兼容 schema 的宿主加载。管理 API 的变更路由由宿主管理 key 保护；插件页面只在浏览器内存中使用用户输入的 key。页面在 key 为空时不会请求任何管理 API，避免被 CLIProxyAPI 的失败计数误封来源 IP。

## 构建与安装

### 从 Release 手动安装

从 [最新 Release](https://github.com/helloshu/cpa-plugin-codex-wakeup/releases/latest) 下载与服务器架构对应的 zip：

```text
codex-wakeup_<version>_linux_amd64.zip
codex-wakeup_<version>_linux_arm64.zip
```

用同一 Release 中的 `checksums.txt` 校验后解压。zip 根目录只有一个 `codex-wakeup.so`，把它复制到 CLIProxyAPI 的 `plugins.dir`，赋予读取/执行权限并重启 CLIProxyAPI。

### 通过自定义插件源安装

本仓库包含 CLIProxyAPI plugin-store 兼容的 `registry.json`。在配置中加入：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/helloshu/cpa-plugin-codex-wakeup/main/registry.json
```

重启或刷新 Management Center 后，可以在插件商店中搜索 `codex-wakeup` 安装或更新。

### 从源码构建

需要 Linux 上可用的 Go 与 cgo。构建命令：

```bash
./scripts/build.sh
```

默认产物为：

```text
dist/linux/amd64/codex-wakeup.so
```

将该 `.so` 复制到 CLIProxyAPI v7 的插件发现目录（通常由 `plugins.dir` 指定的目录）。文件名中的插件 ID 应为 `codex-wakeup`；不要复制构建时生成的临时 `.h` 文件。CLIProxyAPI 启动并发现动态库后，会通过 `plugin.register` 传入配置并注册管理路由。

同时构建 Linux AMD64/ARM64 后，可生成符合 plugin-store 命名规则的 zip 和校验文件：

```bash
./scripts/package-release.sh 0.1.6
```

## 配置示例

下面示例默认每个任务间隔 5 小时，并让一个任务顺序唤醒两个 Codex OAuth 账号。`account_ids` 只使用宿主稳定 `auth_index`，留空则表示全部符合条件的 Codex OAuth 文件账号。

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    codex-wakeup:
      enabled: true
      priority: 1
      auto_wake: true
      scan_interval: 30s
      state_file: plugins/codex-wakeup/state.json
      history_limit: 300
      request_timeout: 60s
      default_model: gpt-5.3-codex
      default_prompt: hi
      default_max_output_tokens: 32 # 兼容旧配置；官方直连不会发送此字段
      run_on_start: false
```

任务可通过页面或管理 API 保存，例如：

```json
{
  "tasks": [
    {
      "id": "two-codex-accounts",
      "name": "两个账号定时唤醒",
      "enabled": true,
      "account_ids": ["auth-index-1", "auth-index-2"],
      "schedule": {"kind": "interval", "interval": "5h"},
      "prompt": "hi",
      "model": "gpt-5.3-codex",
      "max_output_tokens": 32
    }
  ]
}
```

账号在同一任务内串行执行；一个账号失败不会阻塞后续账号。任务和账号有去重锁，重复触发时会记录 `skipped`。任务失败或部分失败同样会推进下一次固定间隔周期。

页面支持五类唤醒条件：

- `daily`：每天在服务器本地时区的 `daily_time` 执行；
- `weekly`：每周在 `weekly_days`（周日为 0、周六为 6）和服务器本地 `weekly_time` 执行；
- `interval`：从任务创建或上次完成时间起按 `interval` 执行，允许 1 分钟至 30 天；
- `quota_reset`：只读查询官方 `https://chatgpt.com/backend-api/wham/usage`，每个账号独立在 `primary_window`、`secondary_window` 或 `either` 指定的真实额度窗口重置 **1 分钟后**执行一次；
- `startup`：每次插件 worker generation 启动后执行一次，可配置 0 至 10080 分钟延迟。

额度查询结果缓存 2 分钟，失败时指数退避到最多 30 分钟；查询失败不会用旧的 reset 时间触发唤醒。`run_on_start` 是旧版全局兼容开关：启用后，尚未运行的普通每日、每周和间隔任务会在 worker 启动时额外执行一次；独立的 `startup` 任务不依赖该开关。

同一个额度重置任务中，账号 A 到期只会唤醒 A；账号 B 按自己的重置时间加 1 分钟独立触发。多个账号都已经到期时，才会在同一轮扫描内依次执行。任务为每个账号保存已处理的重置边界，账号忙碌而跳过时保留待执行状态，不会因其他账号完成而丢失。额度唤醒遇到网络错误、408、429 或 5xx 会按账号退避重试（2、4、8、16、30 分钟，之后保持 30 分钟）；401/403 保留待执行边界，以 30 分钟间隔重新读取宿主凭据后重试。400 等请求参数错误记录失败并结束本次边界，需修正任务配置。成功账号不会随其他账号一起重试。

### 额度耗尽后持续监控

账号标识 `auth_index` 的“稳定”只表示用于识别同一个账号，不表示额度充足。页面和额度监控包含宿主返回的未禁用 Codex OAuth 文件账号；即使宿主标记 `unavailable`，也会继续只读查询额度。普通手动、每日/每周、间隔和启动任务仍遵守宿主的不可用状态；额度重置任务则在已知窗口重置 1 分钟后直接尝试该账号，不等待宿主先清除不可用标记。

每轮扫描重新读取宿主账号列表，已删除或明确 `disabled` 的账号不查询、不唤醒，也不会因为旧缓存产生空执行记录。查询失败会保留历史时间并退避重试，恢复成功后再判断待执行边界，不能用失败查询留下的旧时间直接触发。重试进度存入任务的 `quota_retries`，重启后继续遵守原重试时间；页面显示宿主可用状态、是否加入额度监控、下次查询及各账号的重试时间。

插件不会修改宿主账号的禁用/不可用标记，也不会自行刷新 OAuth token。有效凭据由宿主或用户恢复；恢复后下一次查询/重试会重新读取。自动监控能持续尝试，实际唤醒成功仍取决于凭据、网络与官方服务；插件成功唤醒也不等同于宿主业务路由状态已恢复。

成功刷新额度时，如果官方已将窗口切换到下一周期或清空窗口，插件会保留刚过去的重置时间，再按各任务、各账号的处理进度判断是否需要唤醒，避免刷新覆盖时间导致漏触发。这个边界会随状态持久化，重启后仍可执行未处理的重置；已经处理的重置不会重复执行。worker 启动后会立即扫描已经到期的任务，不必再等待一个 `scan_interval`。自动触发时间包含 1 分钟等待，并受扫描周期和请求耗时影响；默认每 30 秒扫描一次。

## 日志与漏触发排查

- **执行历史**：页面“最近历史”、`GET /v0/management/codex-wakeup/history?limit=300`，以及 `state_file` 的 `history` 字段。默认文件是相对于 CLIProxyAPI 工作目录的 `plugins/codex-wakeup/state.json`，默认保留最近 300 次记录。这里保存已结束的执行，不代表完整的调度日志。
- **宿主日志**：插件通过 `host.log` 写入 CLIProxyAPI 的日志系统，消息以 `codex-wakeup:` 开头；具体输出到控制台还是文件由宿主配置决定，插件不单独创建 `.log` 文件。info 记录调度启停、任务开始和完成；warn 记录额度查询、执行或落盘失败；debug 记录扫描时任务未到期、任务禁用等跳过原因。任务日志带有 `task_id`、`run_id`、触发来源、状态和脱敏结果，不包含 token、提示词或完整响应。
- **调度诊断**：`GET /v0/management/codex-wakeup/diagnostics` 返回 `scheduler_status`、服务器时区、扫描间隔和 `scheduler` 心跳。`last_scan_at` 是最近扫描开始时间，`last_scan_completed_at` 是最近完成时间；`phase` 为 `quota_refresh` 表示正在查询额度，`executing` 表示正在执行 `current_task_id`。如果开始时间长期不更新，或扫描长期未完成，可以结合宿主日志定位停在哪一步。心跳和 `last_error` 为当前配置生命周期内的信息，重新配置或重启会重置；`last_error` 保留最近错误，不代表错误仍未恢复。

自动执行需要插件配置中的 `enabled: true`、`auto_wake: true` 和任务自己的 `enabled: true` 同时成立。`auto_wake` 默认是 `false`，仅在页面启用任务不会开启后台扫描。诊断中的 `scheduler_status` 会明确返回 `plugin_disabled`、`auto_wake_disabled`、`host_unavailable`、`worker_stopped` 或 `running`。

额度重置任务的 `interval: 5h` 是兼容字段，并不表示每隔 5 小时执行；实际依据 `quota_reset_window` 选择的官方窗口。`next_run_at` 是所选账号中最早的未来触发预览，已包含重置后 1 分钟等待。排查漏触发时还应对照账号的 `quota_last_refresh_at`、`quota_next_refresh_at`、`quota_last_error` 和重置时间。账号的 `primary_elapsed_reset_at` / `secondary_elapsed_reset_at` 保存已观察到的过去边界；任务的 `quota_handled_resets` 分账号记录已处理到哪个边界，这些进度由服务器维护，不能通过管理 API 伪造。

旧版本已经覆盖掉的重置时间无法从当前快照还原；升级后从仍可观察到的窗口边界开始保留。需要补做一次唤醒时，可在页面使用“立即执行”。

## 管理页面与 API

资源页面：

```text
/v0/resource/plugins/codex-wakeup/status
```

管理 API 命名空间：

```text
/v0/management/codex-wakeup/overview
/v0/management/codex-wakeup/accounts
/v0/management/codex-wakeup/preview      POST
/v0/management/codex-wakeup/tasks        GET/POST/PUT/PATCH/DELETE
/v0/management/codex-wakeup/save-tasks   POST
/v0/management/codex-wakeup/wake         POST
/v0/management/codex-wakeup/history
/v0/management/codex-wakeup/diagnostics
```

`POST /wake` 支持三种选择：

- `{}`：手动唤醒全部符合条件的 Codex OAuth 文件账号；
- `{"auth_indices":["auth-index-1"]}`：只唤醒指定稳定 `auth_index` 账号；
- `{"task_id":"two-codex-accounts"}`：使用任务自己的 `account_ids`，不意外扩大为全部账号。

也可给 `account_ids`、`prompt`、`model`、`max_output_tokens` 做本次手动请求覆盖。这里保留 `max_output_tokens` 仅为兼容已有任务和调用方；ChatGPT 官方 Codex Responses backend 会拒绝该字段，因此插件不会把它序列化到上游请求中，也不会把它当作硬上限。管理页面及 API 会显示已脱敏的错误、HTTP 状态和耗时，不包含凭据 JSON、Authorization、token 或完整上游响应。

WebUI 可直接新增、编辑、删除、启停和立即执行任务，可多选账号并设置模型、提示词及上述五类条件。编辑器的后续触发预览由服务端计算，因此每日/每周规则使用 CLIProxyAPI 容器的本地时区；返回的绝对时间会在浏览器中换算为浏览器本地时间显示。旧的批量 `save-tasks` API 仍允许空 `account_ids` 表示全部可用账号，而表单式 `POST/PUT /tasks` 要求至少选择一个账号，避免误操作。

上游请求固定发送 `model`、`input`、严格短答 `instructions`、低 reasoning effort、`include: ["reasoning.encrypted_content"]`、`parallel_tool_calls`、`stream` 和 `store`；不会发送 `max_output_tokens`。本请求形状逐字段参考 `jlcodes99/cockpit-tools` 固定提交 `cdcd27c39b0d4433a445b5ac55bd50fbd59eddf8` 的官方直连唤醒实现，插件仍只使用自己的独立实现；其官方 OAuth 直连目标为 `https://chatgpt.com/backend-api/codex/responses`，并使用 `codex-tui` 的请求标识头。

## 状态文件与安全边界

`state_file` 必须是相对于 CLIProxyAPI 工作目录的 `.json` 路径。插件使用 0600 权限、临时文件加 rename 的方式保存，并限制历史数量；损坏的旧文件会改名为 `.corrupt-<timestamp>` 后从空状态继续。状态文件只含任务、脱敏账号标签、时间、计数和有限历史。

插件侧使用 `context.WithTimeout` 控制结果是否接受，但 v7 的 `host.http.do` C 回调本身没有取消句柄。若宿主回调已经进入阻塞网络调用，插件不能强制中断调用，也不能保证它在 `request_timeout` 内返回；超时后返回的结果会被拒绝，shutdown 也可能等待宿主回调返回。该设置不是对宿主底层 socket 的强制取消。

## 验证

```bash
go test ./...
go vet ./...
go test -race ./...
./scripts/build.sh
./scripts/package-release.sh 0.1.6
nm -D --defined-only dist/linux/amd64/codex-wakeup.so | grep cliproxy_plugin_init
sha256sum dist/linux/amd64/codex-wakeup.so
```

测试使用 Go fake host，覆盖两账号串行、账号精确映射、单账号失败隔离、调度推进、ABI wire、状态落盘和脱敏；不会向真实 Codex endpoint 发请求。当前交付未在真实账号上验证，请先使用隔离测试环境。

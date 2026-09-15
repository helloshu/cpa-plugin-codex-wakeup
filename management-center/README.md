# 管理中心配套页面

内嵌鉴权需要同时更新插件和管理中心。插件单独升级无法让官方管理中心自动代理请求。

本目录的补丁基于官方 [Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/tree/c12997e1a544374336ea385d5e9a9bbabe1e4767) 固定提交 `c12997e1a544374336ea385d5e9a9bbabe1e4767`，采用原项目 MIT 许可。只调整 Codex Wakeup 的内嵌入口，并保留其他插件原有加载方式。

管理中心通过已有 API 客户端鉴权加载 `/v0/management/codex-wakeup/ui`，再放入仅允许脚本的 sandbox iframe。页面通过 MessagePort 请求本插件的固定接口；管理 key 留在管理中心，不传入 iframe。退出登录、切换连接或离开页面时关闭通道并取消未完成请求。独立 `/status` 页面仍手动输入 key，仅保存在本页内存中。

## 构建

需要 Git、Node.js 24、Bun 1.3.14：

```bash
bash scripts/build-management.sh
```

脚本下载固定上游源码、应用补丁、运行原项目测试/lint/build，再生成 `dist/management/management.html` 和许可证文件。GitHub CI 使用同样流程；Release 中可直接下载，无需自行编译。

## Podman 部署

从同一 Release 下载架构对应的插件 ZIP、`management.html`、`management.LICENSE.txt` 和 `checksums.txt`，校验所下载文件。解压插件到现有 `plugins.dir` 挂载目录，保留原来的状态文件。管理页面单独挂载，不需要更换 CLIProxyAPI 镜像。

在宿主机准备 `./management/management.html`，将以下配置合并到现有 Podman Compose 服务中（服务名 `cpa` 按实际替换）：

```yaml
services:
  cpa:
    environment:
      MANAGEMENT_STATIC_PATH: /opt/cpa-management/management.html
    volumes:
      - ./management:/opt/cpa-management:ro,Z
```

在现有 CLIProxyAPI `config.yaml` 中合并下面两项，保留原有管理 key 等设置：

```yaml
remote-management:
  disable-control-panel: false
  disable-auto-update-panel: true
```

`disable-auto-update-panel` 防止官方页面自动更新覆盖配套版本。启用 SELinux 的 Podman 主机使用上面的 `:Z` 标签；其他环境可以仅使用 `:ro`。

```bash
podman compose up -d --force-recreate cpa
```

如果使用 `podman run`，重建现有容器时保留原有参数，并添加：

```bash
-e MANAGEMENT_STATIC_PATH=/opt/cpa-management/management.html \
-v /绝对路径/management:/opt/cpa-management:ro,Z
```

更新后刷新 `management.html` 并登录，从插件内嵌入口打开，不再输入第二次 key。独立 `/v0/resource/plugins/codex-wakeup/status` 页面仍可使用，需手动输入 key。配套页面不会调整反向代理、Cloudflare Access 或已有管理 API 的访问规则。

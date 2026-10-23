# AI NFO · NAS 视频资料工作台

在浏览器选择 NAS 视频目录，批量用 AI 整理标题、简介、分类与标签，检查草稿后将 NFO 写回视频旁边。面向 QNAP Container Station 自部署，不需要把视频先上传到网页。

后端使用 Go，前端为 HTML / CSS / JavaScript，SQLite 存储应用数据。项目处于开发阶段，不提供旧版本数据迁移。

## 已实现

- 已挂载目录浏览、相对路径输入、递归扫描（单次最多 5000 个视频）。
- 独立视频、单个系列 / 教程两种扫描方式。支持 `S01E02`、`02-标题` 等明确数字分集；无法识别的集数需手动填写。
- 可配置 Chat Completions 兼容接口或 Ollama。地址、模型、密钥在网页设置；不绑定特定供应商或模型。
- 基于文件名、目录名、同名 `.srt` / `.vtt` / `.txt` / `.description` / `.info.json`、同目录 `README.md` / `简介.txt` / `description.txt` 生成元数据；支持 `视频名.zh.srt` 等字幕命名。
- 草稿编辑、系列简介与分集编辑、XML 预览、批量保存、任务取消、错误信息、重启恢复提示。
- 默认不覆盖草稿或已有 NFO。明确选择覆盖 NFO 时，先在原目录保留唯一 `.bak.*` 备份。视频文件不改名、不移动、不写入。
- 访问密码、服务端密钥保存、路径范围限制、拒绝符号链接、同名不同格式冲突检测。

## QNAP 部署

运行方式：**QNAP 上的容器访问共享文件夹 → 浏览器访问 NAS 的 8787 端口**。这不是一个云端托管网页。

推荐使用下面的 **GitHub 构建 → GHCR 镜像 → QNAP 拉取** 流程。也保留后文的 NAS 本地构建方式，二选一即可。

### 方式一：GitHub 自动构建，QNAP 拉取运行

#### 1. 上传仓库并构建

将项目源码上传到 GitHub 仓库根目录，必须包含隐藏目录 `.github/workflows/docker.yml`。不要上传 `.env`、`data/` 或视频目录；应用运行时的 AI 密钥只需在 NAS 网页里设置，不需要放进 GitHub Secrets 或镜像。

工作流会自动：

1. 检查 Go 格式、运行静态检查与竞态检测测试，检查前端 JavaScript，并构建 Go 运行镜像。
2. 启动临时容器检查健康状态、页面和未登录访问保护。
3. 测试通过后，将默认分支或 `v*` 标签的镜像推送至 GHCR，包含 `linux/amd64` 和 `linux/arm64`。普通分支和 PR 只测试，不发布。

镜像地址自动从仓库名生成并转换为小写。例如仓库为 `your-name/ai-nfo`：

```text
ghcr.io/your-name/ai-nfo:latest          # 默认分支的最新成功构建
ghcr.io/your-name/ai-nfo:v1.0.0         # 推送 v1.0.0 Git 标签后生成
ghcr.io/your-name/ai-nfo:sha-完整提交SHA # 每个发布的提交都保留此标签
```

在仓库 **Actions → Test and publish container** 查看结果。也可以在默认分支上使用 **Run workflow** 手动构建。工作流使用 GitHub 自动提供的 `GITHUB_TOKEN`，并仅在发布任务授予 `packages: write`；通常无需另配发布令牌。如果组织限制 Actions 或包创建权限，需要由仓库 / 组织管理员放开对应权限。

#### 2. 确定镜像访问方式

GHCR 包首次发布通常为私有，**代码仓库公开不代表镜像包已经公开**。在 GitHub 账号 / 仓库的 **Packages → 对应镜像 → Package settings** 检查可见性。

- 如果希望 NAS 无需登录就能拉取，可自行把镜像包设为 Public。
- 如果保持私有，在 NAS 执行 `docker login ghcr.io -u 你的GitHub用户名`，交互提示的密码填有 `read:packages` 权限的 classic PAT。不要把令牌写入 Compose；组织启用 SSO 时还需授权该令牌。通过 Container Station 拉取时，需在它的镜像仓库设置中配置对应凭据，SSH 登录凭据不一定被界面复用。

工作流不会更改仓库或镜像的可见性。有关匿名拉取、令牌与包权限，见 [GitHub Container Registry 文档](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)。

#### 3. 在 QNAP 配置 Compose

NAS 只需要下载 `compose.ghcr.yaml` 和 `.env.example`，放在同一个应用部署目录，不需要完整源码。预先用 File Station 创建应用数据目录，并确认视频目录已经存在。

复制 `.env.example` 为 `.env`，例如：

```dotenv
AI_NFO_IMAGE=ghcr.io/your-name/ai-nfo:latest
NAS_MEDIA_PATH='/share/Multimedia/网络视频'
NAS_DATA_PATH='/share/Container/ai-nfo-data'
APP_PASSWORD='请替换为你自己的至少8位密码'
```

把镜像用户名、仓库名、共享文件夹路径和密码替换为实际值。在这两个配置文件所在目录执行：

```sh
docker compose -f compose.ghcr.yaml pull
docker compose -f compose.ghcr.yaml up -d
```

如 QNAP 仅提供旧版命令，将 `docker compose` 换成 `docker-compose`。`compose.ghcr.yaml` 是独立部署文件，不要和本地构建的 `compose.yaml` 合并使用。

随后在浏览器访问 `http://NAS地址:8787`。若 NAS 无法连接 `ghcr.io`，需要先解决镜像仓库网络可达性。

也可以把 `compose.ghcr.yaml` 内容粘贴到 **Container Station → 创建应用** 的 Compose 编辑器；如果该入口未加载同目录 `.env`，将其中 `${变量:?提示}` 逐项替换为实际值（密码和带特殊字符的路径使用 YAML 单引号）。在 YAML 中直接填写含 `$` 的密码时，按 Compose 规则将 `$` 写成 `$$`。若旧版本不支持 `bind.create_host_path`，先确认源目录已创建，再去掉该子配置。具体界面以安装的 Container Station 版本为准。

#### 4. 更新和回退

推送新代码并等待 Actions 发布成功后，在 NAS 执行：

```sh
docker compose -f compose.ghcr.yaml pull
docker compose -f compose.ghcr.yaml up -d
```

新镜像不会自动更新正在运行的容器，需要执行上述命令或在 Container Station 中重新创建容器。更新前等待批量任务结束；`/data` 和视频目录是宿主机挂载，重建容器会保留其中的数据。

需要固定版本时，将 `.env` 的 `AI_NFO_IMAGE` 改成已经发布的 `:v1.0.0`、`:sha-完整提交SHA` 或 GHCR 给出的 `@sha256:...` 摘要，再拉取并重建。开发阶段不提供跨版本数据迁移或兼容保证；切换数据结构不一致的版本时请使用新的应用数据目录。切换镜像版本不会回滚已经写出的 NFO。

### 方式二：在 NAS 本地构建镜像

### 1. 准备目录

通过 File Station 创建应用源码和应用数据目录，例如：

```text
/share/Container/ai-nfo          # 放这个项目的源码
/share/Container/ai-nfo-data     # 草稿数据库、任务记录、模型配置
/share/Multimedia/网络视频       # 已有的视频目录（示例）
```

**实际路径以你的 QNAP 为准。** 不要直接假定上述目录存在。容器中的服务需要对视频目录有读权限，保存 NFO 还需要写权限；数据目录需要读写权限。不要使用特权容器来绕过目录权限。

### 2. 首次构建镜像

将本项目上传至源码目录。随附 `compose.yaml` 是本地构建方案，需在 QNAP SSH 终端首次运行（Container Station 已安装 Docker）。

```sh
cd /share/Container/ai-nfo
cp .env.example .env
```

编辑 `.env`：填入真实存在的 `NAS_MEDIA_PATH` 和 `NAS_DATA_PATH`，设置至少 8 位 `APP_PASSWORD`。若路径包含空格，可用单引号包裹值；密码含 `$` 等字符时也使用单引号。

```dotenv
NAS_MEDIA_PATH='/share/Multimedia/网络视频'
NAS_DATA_PATH='/share/Container/ai-nfo-data'
APP_PASSWORD='请替换为你自己的密码'
```

启动：

```sh
docker compose up -d --build
```

若该版本仅提供旧版命令，使用 `docker-compose up -d --build`。首次构建需要能拉取 `golang:1.25-alpine`、`alpine:3.22` 并下载 Go 模块依赖。后端使用 Go 与纯 Go SQLite 驱动，网页资源嵌入单个可执行文件；运行镜像无需语言运行时。不同 Container Station 版本的 Compose 支持可能不同，容器镜像尚需在实际 QNAP 上构建验证。

之后打开 `http://你的NAS地址:8787`，使用 `.env` 的访问密码登录。SSH 只用于首次构建、升级和配置，日常目录扫描、模型设置、草稿与批量操作均在网页完成。

### 也可以在 Container Station 界面创建容器

先在 NAS 的项目目录构建镜像：

```sh
docker build -t ai-nfo:local .
```

在 Container Station 中从本地镜像 `ai-nfo:local` 创建容器：

| 配置 | 值 |
|---|---|
| 端口 | NAS `8787` → 容器 `8787` TCP |
| 环境变量 | `APP_PASSWORD`：自行设置至少 8 位密码 |
| Bind Mount Host Path | 视频共享文件夹 → `/media`，读写 |
| Bind Mount Host Path | 应用数据文件夹 → `/data`，读写 |
| 重启策略 | Unless stopped / 除非手动停止 |

其余环境变量镜像已设置。不要另外启动一份使用同一个 `/data` 的实例。端口已被占用时可换 NAS 端口，容器端口保持 8787。

仅使用网页无法完成“把未挂载的 NAS 目录授权给容器”；第一次需要设置挂载。网页看到的 `/media` 对应挂载的视频共享文件夹，输入 `教程/Python` 等相对路径即可选取子目录。若要访问多个共享文件夹，可分别映射到 `/media/目录A`、`/media/目录B`，避免映射整个 NAS 根目录。

## 日常使用

1. **模型设置**：选择接口类型，填写基础地址和模型 ID，保存并测试。
   - 兼容接口：程序追加 `/chat/completions`；例如某服务要求 `/v1/chat/completions`，基础地址就填写到 `/v1`。服务不支持 `response_format` 时关闭 JSON 模式。
   - Ollama：填写 `http://局域网模型主机:11434`，程序追加 `/api/chat`。容器内 `localhost` 指本容器，不是另一台电脑或 NAS 宿主机。
   - 测试连接会真实发送一次小请求，可能产生模型费用。没有内置或免费提供模型。
2. **添加视频目录**：独立视频可扫描一个大目录；系列模式要选择**一套**教程的根目录，多个系列分别扫描。选错类型时，可选择正确类型重新扫描，更新分类和所属系列。视频草稿保留；分类或所属系列变化时重新识别季数、集数，已保存的条目恢复为待确认草稿，需要检查后重新保存 NFO。同类型、同系列重新扫描保留手动修改的季数、集数。
3. **选中 → AI 生成**：生成草稿，不直接写文件。整个系列会先生成一次系列简介，再逐集生成。
4. **编辑资料**：检查简介和分集信息，可查看实际提供给 AI 的输入片段；保存草稿或预览 XML。
5. **保存 NFO**：默认跳过已有文件。勾选覆盖时先备份原 NFO。没有草稿、无权限、集数缺失或冲突的条目会失败，其余条目继续处理。

关闭浏览器不影响任务；取消会等待当前 AI 请求或文件操作结束（AI 请求超时为 120 秒）。单实例一次运行一个批量任务，避免 NAS 压力和写入冲突。重启时未完成任务标记为中断，已完成草稿和 NFO 保留；重新筛选剩余条目执行即可。

保存失败并不会回滚已经写入的其他文件。例如系列总览成功而单集写入失败时，总览会保留。默认跳过模式可以安全重试；覆盖模式每次覆盖都会留下新备份。

## NFO 输出

独立视频使用 `<movie>`：

```text
摄影技巧.mp4
摄影技巧.nfo
```

系列使用 `<tvshow>` + `<episodedetails>`：

```text
Python 教程/
  tvshow.nfo
  Season 01/
    Python S01E01 - 环境准备.mp4
    Python S01E01 - 环境准备.nfo
    Python S01E02 - 变量.mp4
    Python S01E02 - 变量.nfo
```

使用本地 `uniqueid`，不伪造 TMDb / IMDb 等 ID。XML 由程序生成和转义。没有日期、评分或作者证据就省略对应字段。

这是 **Kodi 风格的通用 XML**，尚未针对某个用户选定媒体库做实机验证。各播放器的导入设置、命名规则和字段支持不同。剧集通常仍需 `S01E01` 等命名让媒体库正确发现；本工具不会自动改名，仅有 NFO 不保证任意文件名都能入库。多集合并文件目前不支持；同目录同名不同扩展名视频会共用 NFO，因此先提示冲突。

## 数据与当前边界

- 密钥以权限 `0600` 的 JSON 保存在 `/data/settings.json`，没有加密；网页仅收到“是否已设置”，不会回显密钥。保护应用数据目录及其备份。
- 远程模型会收到选中视频的文件名、父目录名和截取的字幕 / 简介。单视频合计最多 24000 字符，单个资料文件最多读取 160000 字节；长资料不是全文摘要，生成内容需核对。
- 不会上传视频；不处理内嵌字幕、不提取音频或画面、不联网抓取原视频网页、不生成海报。没有外置资料时只能进行保守的文件名整理。
- 已有 NFO 会被检测和保护，**不会自动导入为工作台草稿**。手动选择覆盖会用工作台当前字段重新生成 NFO，原有其他字段在 `.bak.*` 中保留。
- 视频库底部提供“清理不可访问视频”入口：先列出整个视频库的待清理记录，确认后移除记录和草稿（每次最多 5000 条），不删除视频、字幕或 NFO。可用“文件不可访问”筛选查看这些条目。确认时会重新检查，已恢复访问的文件自动跳过；批量任务运行中或视频根目录不可访问时不能清理。数据目录为单个视频根目录服务，换挂载内容时请使用不同的数据目录，避免旧路径草稿被复用。
- 默认局域网使用。若需外网访问，在 QNAP 反向代理配置 HTTPS，勿直接把明文 HTTP 端口开放至公网。
- 图片、自动转写、NFO 导入合并、批量重命名和媒体库远程刷新尚未实现。

## 本机开发与验证

Go 1.25 或更新版本；首次构建会下载 `go.mod` 中的依赖。默认仅绑定本机地址；对局域网监听必须设置至少 8 位访问密码。

```sh
MEDIA_ROOT=/实际的视频目录 DATA_DIR=/实际的数据目录 go run .
```

```sh
go test -race ./...
go vet ./...
go build -o ai-nfo .
node --check static/app.js
```

测试使用临时目录和本地假模型 HTTP 服务，验证目录边界、真实协议请求、草稿到 XML 的文件写入、跳过和备份、冲突、任务取消与中断恢复、身份验证及跨站请求拒绝。假模型只用于测试，不会在产品中伪装成真实生成。

### VS Code 断点调试

1. 用 VS Code 打开项目根目录，安装推荐的 **Go** 扩展（`golang.go`），确保终端可以执行 `go version`。
2. 在项目根目录创建 `media` 文件夹，放入用于测试的视频及字幕 / 简介。`data` 目录由程序启动时自动创建；这两个目录都已被 Git 忽略。
3. 在“运行和调试”中选择 **AI NFO：本地启动**，按 `F5`。首次启动若提示安装 Delve，按提示安装 `dlv` 后再启动。
4. 浏览器打开 `http://127.0.0.1:8787`，可在 Go 代码中设置断点。默认仅监听本机、无需登录；AI 接口在网页设置。

配置位于 `.vscode/launch.json`。修改 `MEDIA_ROOT` 可指定其他本地目录，修改 `PORT` 可避开端口冲突；如需测试登录，设置其中的 `APP_PASSWORD`。该配置直接提供环境变量，不读取部署用的 `.env` 文件。

选择 **AI NFO：调试测试** 可调试整个包的测试，或使用 Go 扩展在具体测试函数上方提供的调试入口。测试内有较短的等待时间，长时间停在异步任务断点上可能触发测试超时。网页资源嵌入 Go 程序，修改 HTML / CSS / JavaScript 后需重新启动调试并刷新浏览器。

调试方式参见 [VS Code Go 官方文档](https://github.com/golang/vscode-go/blob/master/docs/debugging.md)。

## 参考

- [QNAP Container Station 3：目录挂载和容器管理](https://www.qnap.com/en-ca/how-to/tutorial/article/how-to-use-container-station-3)
- [Kodi NFO 格式](https://kodi.wiki/view/NFO_files)
- [Jellyfin 本地 NFO](https://jellyfin.org/docs/general/server/metadata/nfo/)
- [DeepSeek Chat Completions 协议](https://api-docs.deepseek.com/api/create-chat-completion/)
- [Ollama Chat API](https://docs.ollama.com/api/chat)

# pt cli 发布流程

发布源版本在仓库根目录 `VERSION`，当前是 `v0.4.0-alpha`。CI 预览身份为
`<VERSION>+g<实际测试提交前12位>`；发布身份必须与已存在的 tag 和该提交的
`VERSION` 完全一致。接受 `vMAJOR.MINOR.PATCH` 和 `-rc.1`、`-alpha` 等
SemVer prerelease，不接受发布 tag 的 build metadata 或前导零。

## 维护者操作

1. 通过正常 PR 更新 `VERSION`、说明和代码，检查 CI，再经授权合入 main。
2. 确认要发布的 main-line commit、版本和授权后，创建并推送该版本的 tag。
   推荐 annotated tag。此步骤由维护者显式执行，工作流从不创建或移动 tag。
3. `v*` tag push 自动触发 `release draft`。也可以在 Actions 手动运行它，
   输入一个已经存在的 tag。手动触发需要此工作流已进入默认分支。
4. 等待整个 run 成功，再检查 Draft Release 的三个 ZIP、`SHA256SUMS` 和
   `release-manifest.json`，核对 tag、完整 commit、CI、下载后校验与发行说明。
5. **公开发布是独立的人工操作**。工作流只创建 draft，不自动发布、不合并 PR，
   不设置 latest。带 prerelease 后缀的 tag 会标记为 prerelease。

## 构建与验收

CI 和发布都调用 `build.yml`。只支持经过原生运行验收的目标：

| 系统 | 架构 | 产品文件 |
| --- | --- | --- |
| Windows | amd64 | `pt-cli-<version>-windows-amd64.zip` |
| Linux | amd64 | `pt-cli-<version>-linux-amd64.zip` |
| macOS | arm64 | `pt-cli-<version>-darwin-arm64.zip` |

每个原生 runner 执行格式、Python 发布控制回归、Go vet/race 测试；以同一源码
和工具链两次构建 `pt` / `ptctl`，比较打包后的完整字节。使用 `-trimpath`、
`-buildvcs=false`、空 build ID 和 `CGO_ENABLED=0`，注入明确 version/commit。
Go 版本由 `go.mod` 固定，自动工具链切换关闭，实际 Go 版本记入 `build.json`。

ZIP 使用排序条目、固定 1980 时间、固定文件权限和 STORE 编码，避免文件 mtime、
遍历顺序和压缩库版本影响。代价是包比压缩 ZIP 大。复现条件是同一源码、目标、
Go 工具链和构建参数；双构建可复用编译缓存，不是两家独立构建服务的证明。
跨系统的二进制不同，不声称它们彼此字节相同。

Windows 使用 PowerShell `Expand-Archive`；macOS/Linux 使用系统 `unzip`。
解包后核对每个文件，并运行既有双入口合成验收。三平台全部成功后才汇总资产；
汇总检查目标集合、完整 commit、版本、channel 和全部内外校验值。普通 CI 也
运行这条资产汇总路径，产出 14 天的 preview artifacts，但没有 Release 写权限。

## 权限与失败语义

默认 `contents: read`，仅依赖全部验证成功的最后一个 draft job 使用
`contents: write`。只使用内置 `GITHUB_TOKEN`，checkout 不保留凭据；不要求 PAT。
tag 必须解析到 main 历史中的提交，且其 `.github/workflows` 与当前 main 一致。
这也避免 GitHub 创建 Release 时对不同工作流版本要求额外 workflow 写权限，
详见 [GitHub Release API](https://docs.github.com/en/rest/releases/releases#create-a-release)。
上传前再次读取 tag，上传后再核对，移动或不匹配会失败。

缺失 tag、版本不符、非 main 提交、任一测试失败、构建不一致、缺包、校验失败
都会阻止 Release 创建。已存在的 draft 或公开 Release 均拒绝覆盖。资产上传
按新 draft 的 ID 进行，并核对服务端 digest/state；网络中断或部分上传失败时，
留下未公开的 draft 供维护者检查，不自动重试、删除、替换或公开发布。修复后
需先人工处理已有的不完整 draft，再 **Re-run all jobs**；不要只重跑个别 job，
因为资产名包含 run attempt，以免混用不同轮次的包。

这套流程的接入不等于已经创建正式 tag 或 Release。Release API 写入路径采用
无网络的模拟回归验证；首次实际 draft 上传仍需在维护者授权的真实 tag 上确认。
没有代码签名、Windows 签名或 macOS notarization；没有验证真实下载器凭据或任务。

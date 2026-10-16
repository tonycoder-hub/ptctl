# pt cli 发布流程

发布由合入 main 触发，版本号以仓库根目录 `VERSION` 为准。CI 预览身份为
`<VERSION>+g<实际测试提交前12位>`；发布身份就是该提交的 `VERSION`。接受
`vMAJOR.MINOR.PATCH` 和 `-rc.1`、`-alpha.1` 等 SemVer prerelease，不接受
build metadata 或前导零。

## 维护者操作

1. 在一个正常 PR 里把 `VERSION` 改成新版本，并在 `CHANGELOG.md` 里加一节
   `## <VERSION>`，写这一版的发行说明。新版本必须按 SemVer 高于所有已有的
   `v*` tag。PR CI 会检查当前 `VERSION` 在 `CHANGELOG.md` 里恰好有一节且非空，
   缺了就失败。
2. CI 全绿后合入 main。合入后 `release` 工作流自动运行：三平台测试与构建、
   创建 annotated tag `<VERSION>`、上传资产并公开发布 Release。
3. 不改 `VERSION` 的普通合入也会触发 `release`，但只做判定：该版本的 tag
   已在更早的提交上，于是跳过构建和发布。
4. 发布说明是 `CHANGELOG.md` 对应一节，后接 GitHub 自动生成的 PR 列表和
   完整 commit。带 prerelease 后缀（含 `-`）的版本标记为 prerelease，不设为
   latest；正式版本设为 latest。

不需要手动打 tag；工作流从不移动已有 tag。也可以在 Actions 对 main 手动运行
`release`，它按同样规则判定当前 main。

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

默认 `contents: read`，仅依赖全部验证成功的最后一个 publish job 使用
`contents: write`。只使用内置 `GITHUB_TOKEN`，checkout 不保留凭据；不要求 PAT。
发布的提交必须在 main 历史中，且其 `.github/workflows` 与当前 main 一致。
这也避免 GitHub 创建 Release 时对不同工作流版本要求额外 workflow 写权限，
详见 [GitHub Release API](https://docs.github.com/en/rest/releases/releases#create-a-release)。
`release` 工作流串行运行，排队中的较新 run 会取代较早的待运行 run；较新的
提交只要仍是同一 `VERSION`，就由它发布这一版。

判定在 `plan` 一步完成：`VERSION` 的 tag 在更早的提交上就跳过；没有 tag 时，
`VERSION` 低于或等于任一已有 tag、缺少发行说明、提交不在 main 上，都会让
run 失败而不发布。发布按固定顺序写入，任一步失败即停止：

1. 本地核对 main 历史、三平台资产、清单和全部校验值；
2. 确认没有同名 Release（含 draft），已存在则拒绝覆盖；
3. tag 不存在就创建 annotated tag；已存在则必须指向同一提交，否则失败；
4. 创建 draft，逐个上传资产并核对服务端 digest/state；
5. 再次读取 tag，确认上传期间没有移动；
6. 最后才把 draft 公开，并核对公开后的状态和资产。

因此失败最多留下 tag 和一个未公开的 draft，不会有资产不全的公开 Release。
不自动重试、删除或替换。修复后先人工删除不完整的 draft（tag 保留），再对该
run **Re-run all jobs**；tag 已在本提交上时会继续完成发布。不要只重跑个别
job，因为资产名包含 run attempt，以免混用不同轮次的包。

Release API 写入路径采用无网络的模拟回归验证，首次真实运行是
`v0.4.0-alpha.1`。没有代码签名、Windows 签名或 macOS notarization；没有验证
真实下载器凭据或任务。

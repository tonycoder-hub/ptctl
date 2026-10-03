# pt cli：只读入门

主命令是 `pt`，旧入口 `ptctl` 使用同一套实现。Windows 包同时提供
`pt.exe` / `ptctl.exe`。核心校验与对账逻辑在 Windows、macOS、Linux 共用；
本文用 PowerShell 演示，系统路径、权限和打包属于兼容边界。本文只用 `version`、`torrent inspect/verify`、
`client list` 和 `reconcile report`；不需要初始化私有 store。
这些核心命令可用 `--help` 查看参数，帮助不会读取文件、凭据或连接下载器。

## 1. 获取并校验包

在 [GitHub Actions](https://github.com/tonycoder-hub/ptctl/actions) 选择目标
commit **整体成功**的 `ci` run，下载 `pt-cli-bundle-<commit>-<attempt>`
artifact，或单独的 `pt-cli-windows-amd64-<commit>-<attempt>` artifact。
bundle 同时含 Windows amd64、Linux amd64、macOS arm64 包。PR run 的版本标识
是实际测试的 merge commit。artifact 保留 14 天。已正式发布的版本可以从
仓库 Releases 下载对应 ZIP、`SHA256SUMS` 和 `release-manifest.json`；维护者
创建的 Draft 不等于已公开发布。所有包均无代码签名或 macOS notarization。

先解开 GitHub 下载的 artifact 外层 ZIP；其中是产品 ZIP 和 `SHA256SUMS`。
在该目录打开 PowerShell，验证并解包：

```powershell
$entries = Get-Content .\SHA256SUMS | ForEach-Object {
    $parts = $_ -split '  ', 2
    [PSCustomObject]@{ Hash = $parts[0]; Name = $parts[1] }
}
$package = @($entries | Where-Object { $_.Name -match '^pt-cli-.*-windows-amd64\.zip$' })
if ($package.Count -ne 1) { throw 'Expected exactly one Windows amd64 package' }
if ((Get-FileHash -LiteralPath $package[0].Name -Algorithm SHA256).Hash.ToLowerInvariant() -cne $package[0].Hash) { throw 'ZIP checksum mismatch' }
Expand-Archive -LiteralPath $package[0].Name -DestinationPath .\pt-cli
Set-Location .\pt-cli
Get-Content .\SHA256SUMS | ForEach-Object {
    $entry = $_ -split '  ', 2
    if ((Get-FileHash $entry[1] -Algorithm SHA256).Hash.ToLowerInvariant() -cne $entry[0]) { throw "File checksum mismatch: $($entry[1])" }
}
.\pt.exe version
.\pt.exe version --output json
```

核对输出 `product=pt cli`、预览版 `version=<VERSION>+g<commit-prefix>`（发布包为
与 tag 完全一致的版本）和完整 `commit`。`build.json` 也包含同样身份。校验值用于检测传输损坏；下载来源
仍应是你选择的仓库 CI run。这些 SHA-256 在 CI 生成，源码 checkout 没有预生成二进制。

macOS arm64 选择 `*-darwin-arm64.zip`，Linux amd64 选择 `*-linux-amd64.zip`。
先用 `shasum -a 256`（macOS）或 `sha256sum`（Linux）核对下载 ZIP 与清单中
同名项的摘要，再用 `unzip PACKAGE.zip -d pt-cli` 解压。进入目录后执行
`shasum -a 256 -c SHA256SUMS` 或 `sha256sum -c SHA256SUMS`，然后运行
`./pt version`。ZIP 保留 Unix 可执行权限；当前不提供 macOS Intel 或其他架构包。

## 2. 不连下载器，先用 24 字节样本

包内 `examples/readonly/demo.torrent` 是合成的单文件 v1 元文件：没有 tracker、
passkey、账户或真实下载任务。无需 Python、Go 或网络即可运行：

```powershell
.\pt.exe torrent inspect --output json .\examples\readonly\demo.torrent
.\pt.exe torrent verify --content .\examples\readonly\demo.txt --output json .\examples\readonly\demo.torrent
.\pt.exe reconcile report --torrent .\examples\readonly\demo.torrent --source .\examples\readonly\demo.txt --output json
```

预期：inspect 给出 `name=demo.txt`、`version=v1`、精确 infohash；verify 给出
`verified=true`，`pieces_expected=pieces_matched=1`。local-only reconcile
给出 `outcome=partial`、`storage.status=verified_exact_root`、`writes_performed=0`，
因为尚未提供下载器账本。精确摘要见样本旁的 `expected.json`。

换成自己的文件时，`--content` / `--source` 对单文件 torrent 指向该文件，
对多文件 torrent 指向包含其相对路径的根目录。把所有选项放在位置参数
`FILE.torrent` 前面。读取可能更新 atime 或触发云文件下载。

macOS/Linux 使用同一组命令参数：将 `.\pt.exe` 换为 `./pt`，本地路径改用
对应系统的路径。源代码中的样本以 base64 保存，CI 包附带解码后的 `.torrent`。

## 3. 下载器只读对账

先查看账本，再做当前文件、typed infohash 与路径映射的联合对账。以下示例
假设本机 `D:\Downloads` 对应下载器的 `/downloads`；应按实际挂载关系修改。
密码经交互提示读入内存，再通过 stdin 传递，不写入命令参数或配置文件：

```powershell
$credential = Get-Credential -UserName 'your-user' -Message 'Downloader read-only access'
$credential.GetNetworkCredential().Password | .\pt.exe client list --driver qbittorrent --url http://127.0.0.1:8080 --username $credential.UserName --password-stdin --output json
$credential.GetNetworkCredential().Password | .\pt.exe reconcile report --torrent .\release.torrent --source 'D:\Downloads\release.bin' --host-root 'D:\Downloads' --client-root /downloads --client-style posix --driver qbittorrent --url http://127.0.0.1:8080 --username $credential.UserName --password-stdin --require-reconciled --output json
Remove-Variable credential
```

Transmission 用 `--driver transmission --url http://127.0.0.1:9091/transmission/rpc`；
其精确身份能力限于 v1。远端地址必须使用 HTTPS，明文 HTTP 只接受数字 loopback
地址。如果下载器使用 Windows 路径，设置 `--client-style windows` 和对应
`--client-root`。文件必须是完整内容，下载器任务须处于完整做种状态。

`client list` 和普通 `reconcile report` 只读任务状态；后者在本地证明前后读取
下载器账本。认证会创建会话，但不会添加、启动、暂停、重校验或删除任务。
`consistent` 仅表示本次观察的相关账本一致，不是站点做种资格、连续状态或
私有元文件完全相同的证明；`metafile_variant_relation` 仍可能是 `unobservable`。
路径不同会报告 `different_location` 和明确 blocker；顶层 outcome 为 `partial`，
使用 `--require-reconciled` 时返回 4。

## 4. 结果与退出码

| 退出码 | 含义 |
| --- | --- |
| `0` | 请求成功并输出报告；普通 report 的 `partial` 也可能返回 0 |
| `1` | 读取、认证、网络等操作失败 |
| `2` | 参数使用错误 |
| `3` | 精确内容或元数据完整性不匹配，verify 先输出结果 |
| `4` | 使用 `--require-reconciled`，但对账不是 `consistent`；仍先输出报告 |

PowerShell 用 `$LASTEXITCODE` 查看原生程序退出码。自动化应同时检查退出码
和 JSON 的 `data.outcome` / `data.verified`。JSON schema 保持 `ptctl.dev/v1`。

本地与三平台 CI 对两个入口使用相同合成验收：帮助、inspect、v1 跨文件 piece
与空文件、含 piece layer 的 v2、hybrid 双哈希联合校验、同尺寸损坏、退出码 0–4、
loopback qBittorrent/Transmission 的一致对账及路径冲突、请求白名单、
默认隐私字段和样本零写入。三个平台均使用系统解压工具、核对文件校验值，运行交付包验收。
验收不连接真实客户端，不提供真实凭据。完整说明见仓库 README。

# 从国内镜像源拉取依赖镜像并打上标准标签。
#
# 为什么需要：docker.io 直连常被断（registry-1.docker.io: EOF），
# 但 compose 文件应保持标准镜像名以便在网络正常的环境直接可用。
# 折中方案：从镜像源拉取，再 tag 成 docker.io/... 的标准名。
#
# 用法：  pwsh scripts/pull-images.ps1
#
# 若某个镜像源失效，改 $mirror 变量即可。备选：
#   docker.1ms.run
#   dockerproxy.net
#   hub.rat.dev

$ErrorActionPreference = "Stop"
$env:PATH = "$env:LOCALAPPDATA\Programs\Podman;$env:PATH"

$mirror = "docker.m.daocloud.io"

# (镜像源路径, 标准名) —— quay.io 通常可直连，无需代理
$images = @(
    @("library/redis:7-alpine",                    "docker.io/library/redis:7-alpine"),
    @("library/postgres:16-alpine",                "docker.io/library/postgres:16-alpine"),
    @("minio/minio:RELEASE.2023-03-20T20-16-18Z",  "docker.io/minio/minio:RELEASE.2023-03-20T20-16-18Z"),
    @("apache/kafka:3.8.0",                        "docker.io/apache/kafka:3.8.0"),
    @("milvusdb/milvus:v2.4.15",                   "docker.io/milvusdb/milvus:v2.4.15")
)

Write-Host "pulling via $mirror ..." -ForegroundColor Cyan

$jobs = $images | ForEach-Object {
    Start-Job -ScriptBlock {
        param($mirror, $path, $canonical)
        $env:PATH = "$env:LOCALAPPDATA\Programs\Podman;$env:PATH"
        $src = "$mirror/$path"
        $out = podman pull $src 2>&1
        if ($LASTEXITCODE -ne 0) {
            return "FAIL  $src : $($out | Select-Object -Last 1)"
        }
        podman tag $src $canonical 2>&1 | Out-Null
        return "OK    $canonical"
    } -ArgumentList $mirror, $_[0], $_[1]
}

$jobs | Wait-Job -Timeout 900 | Out-Null
$jobs | ForEach-Object { Receive-Job $_ }
$jobs | Remove-Job -Force

# etcd 走 quay.io，通常可直连
Write-Host "`npulling etcd from quay.io ..." -ForegroundColor Cyan
podman pull quay.io/coreos/etcd:v3.5.5 2>&1 | Select-Object -Last 1

Write-Host "`ndone. now run: podman-compose up -d" -ForegroundColor Green

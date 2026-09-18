param(
    [string]$Protoc = "protoc",
    [string]$GoGenerator = "protoc-gen-go",
    [string]$GRPCGenerator = "protoc-gen-go-grpc"
)
$ErrorActionPreference = "Stop"
$compiler = (Get-Command $Protoc -ErrorAction Stop).Source
$goPlugin = (Get-Command $GoGenerator -ErrorAction Stop).Source
$grpcPlugin = (Get-Command $GRPCGenerator -ErrorAction Stop).Source
if ((& $compiler --version) -ne "libprotoc 28.3") { throw "Requires protoc 28.3" }
if ((& $goPlugin --version) -notmatch '^protoc-gen-go(?:\.exe)? v1\.36\.11$') { throw "Requires protoc-gen-go v1.36.11" }
if ((& $grpcPlugin --version) -ne "protoc-gen-go-grpc 1.6.2") { throw "Requires protoc-gen-go-grpc 1.6.2" }
$wirePath = Join-Path $PSScriptRoot "wire"
New-Item -ItemType Directory -Path $wirePath -Force | Out-Null
$generatorArguments = @("--plugin=protoc-gen-go=$goPlugin", "--plugin=protoc-gen-go-grpc=$grpcPlugin", "--proto_path=$PSScriptRoot", "--go_out=$wirePath", "--go_opt=paths=source_relative", "--go-grpc_out=$wirePath", "--go-grpc_opt=paths=source_relative", (Join-Path $PSScriptRoot "extension.proto"))
& $compiler @generatorArguments
if ($LASTEXITCODE -ne 0) { throw "Protocol generation failed ($LASTEXITCODE)" }
$v1Path = Join-Path $PSScriptRoot "../v1"
& $compiler "--plugin=protoc-gen-go=$goPlugin" "--plugin=protoc-gen-go-grpc=$grpcPlugin" "--proto_path=$PSScriptRoot" "--proto_path=$v1Path" "--go_out=$wirePath" "--go_opt=paths=source_relative" "--go-grpc_out=$wirePath" "--go-grpc_opt=paths=source_relative" (Join-Path $PSScriptRoot "transport.proto")
if ($LASTEXITCODE -ne 0) { throw "Transport protocol generation failed ($LASTEXITCODE)" }

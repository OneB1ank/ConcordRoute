# v2 容器隔离

v2 请求预处理可选择 Linux 容器运行器，在 Linux/Docker 或 Windows/Docker Desktop 的 Linux 引擎上执行。普通进程模式仍是兼容默认值，**普通进程不是操作系统沙箱**。容器模式需要静态 Linux 运行时；当前不隔离 v1 出站传输插件，也不为插件开放网络。

## 准备隔离镜像

在仓库根目录执行：

```powershell
docker build -f deploy/Dockerfile.plugin-sandbox -t sub2api-plugin-sandbox:1 .
```

镜像只包含宿主维护的监督/桥接程序。宿主检查监督 API 标签和 Linux 架构，并以解析到的镜像 ID 创建容器。不会自动拉取镜像，不支持远程 Docker endpoint，不向插件挂载 Docker socket。镜像属于宿主信任范围，应使用仓库提供的最小镜像。

## 启用配置

在宿主配置中设置：

```yaml
plugins:
  v2_sandbox:
    mode: container
    image: sub2api-plugin-sandbox:1
    memory_mb: 256
    cpu_milli: 1000
    pids_limit: 64
```

重启宿主使部署配置生效，再安装包含 `linux-amd64` 或 `linux-arm64`（与宿主 Go 架构一致）运行时的 v2 包。从普通进程模式切换时，应先停用旧插件并重新安装对应运行平台的包；不能将 Windows PE 当作 Linux 运行时。

宿主需要访问本机 Docker Engine。若镜像、Docker、平台、校验或限制初始化失败，启动直接失败，**不会回退到普通进程**。

## 隔离边界

| 控制 | 实现 |
| --- | --- |
| 身份 | 容器内可信 PID 1 将插件降为 UID/GID 65532，清空补充组；插件有效 capabilities 为零 |
| 文件 | 根文件系统和经过 SHA-256 复验的程序副本只读；仅挂载程序与只读租约文件，不暴露宿主目录 |
| 临时数据 | `/tmp` 16 MiB、`/rpc` 1 MiB 的独立 tmpfs，noexec/nosuid/nodev；容器删除后丢弃 |
| 网络 | `--network none`，只有容器回环接口；插件不能自行访问外部 Provider 或宿主服务 |
| 环境 | 宿主和监督程序两层白名单，仅传握手、公钥证书及 HOME/TMPDIR/GOMAXPROCS |
| 内存 | 64–4096 MiB，默认 256；swap 总额与内存相同，避免额外 swap |
| CPU | 100–4000 milli-CPU，默认 1000（一个 CPU） |
| 进程/线程 | cgroup pids 上限 32–256，默认 64；文件描述符上限 256 |
| 生命周期 | 宿主每秒续租；15 秒未观察到更新时监督进程终止插件并退出 PID 1，避免宿主异常结束后遗留进程 |

只有监督程序保留切换 UID/GID 所需的两个 capabilities。插件启动前完成身份下降，不以容器 root 身份执行。宿主限制控制桥接的连接数和地址，只接受容器 `/rpc/` 下的 Unix socket。gRPC 使用自动双向 TLS；插件未获得宿主证书私钥。

控制路径为：宿主本机受 TLS 保护的 gRPC 连接 → Docker exec 标准输入/输出桥接 → 容器 Unix socket → 插件。此路径使插件在没有网络接口的条件下保持 RPC 通信。

资源和网络实现可对照 Docker 的 [资源约束](https://docs.docker.com/engine/containers/resource_constraints/) 与 [none 网络](https://docs.docker.com/engine/network/drivers/none/) 文档。实际限制以本项目下述运行测试为证据。

## 验证

在 `backend` 目录执行：

```powershell
$env:SUB2API_TEST_SANDBOX_CONTAINER = '1'
go test ./internal/pluginruntime ./internal/service -run 'TestContainerIsolationIntegration|TestPluginExtensionContainerProcessIntegration' -count=1 -v -timeout=8m
```

测试覆盖真实子进程的 UID/GID、补充组、capabilities、只读文件、宿主文件/环境不可达、仅回环接口、内存/CPU/pids 内核值、超限分配产生的 Docker OOM 事件，以及停止租约后自动退出。服务层还验证签名安装、改写/拒绝/超时、双实例恢复、热升级、回滚和细粒度路由同步。

2026-09-17 已实际验证 Windows 宿主和 Linux 宿主进程两种控制端，均使用 Docker Desktop Linux Engine。Linux 控制端在可信测试容器中运行，经本机 Unix socket 连接 daemon，并使用受控共享工作目录。可从仓库根目录复现：

```powershell
./backend/internal/pluginruntime/test-linux-host.ps1
```

可信测试宿主使用 Docker socket 和只读源码/模块缓存；**上传的插件容器不获得这些挂载**。脚本仅创建带随机名称和归属标签的临时卷，结束后核对标签并删除。

资源测试使用 64 MiB / 1000 milli-CPU / 64 pids；超额内存分配产生已核验的 Docker OOM 事件，仅终止插件，宿主测试进程继续运行。停止续租后监督进程也能自动终止插件。测试完成后没有遗留该运行器的容器或验证卷。独立 Linux 发行版、额外 LSM 策略和其他架构应在目标环境执行相同验收。

容器隔离依赖宿主内核和 Docker 的安全边界，不代表插件业务逻辑可信。当前能力未授予网络或秘密代理权限；配置和请求体仍应遵守插件声明的最小授权范围。

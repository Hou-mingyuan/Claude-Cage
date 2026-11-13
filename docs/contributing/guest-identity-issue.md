# [Contribution] 为新环境生成常见 PC 风格主机名与独立机器身份

### 要解决的问题与关联 Issue

新建环境存在可识别的固定身份字段。当前主线的 Linux 容器主机名固定为 `cage`，客机与网关 MAC 固定；Lima 主机名默认带 `lima-` 前缀，默认 MAC 使用固定前三字节，且依赖宿主机器身份和实例路径。同一路径重建时，部分标识可重复。Windows 评估版安装模板还固定使用 `CAGE-WIN-EVAL`。

希望新环境获得独立随机身份，同一环境在重启、重连和资源修改后保持稳定；克隆时重新生成目标身份，并兼容已有环境。

关联 Issue：如已有相关讨论，请在此补充编号。

### 基于的 Cage 版本或提交号

df4984ce03923bb36adce31faeb38ec80f0003f1

### 涉及的主机系统

Linux、macOS、Windows。

### 涉及的客机系统

Linux、macOS、Windows。

### 系统版本与架构

实现使用现有 Go 工具链和各平台后端支持的接口。实际验证环境与未验证平台见下文，不能据此推断已完成三平台启动验证。

### 方案内容与提交材料

- 主机名混合 `DESKTOP-` / `LAPTOP-` 常见形态与真实机型/主板简称，附加 6–7 位随机字母数字；大小写与长度可以变化，完整名称不超过 15 字符。
- 内置 1,593 条公开硬件型号记录、1,181 个简称，来自 Lenovo、Dell、HP 与 ASRock 官方资料。创建时不联网、不读取宿主硬件；离线编译器和来源哈希便于更新核对。
- 使用 `crypto/rand` 生成名称各部分、UUID，以及独立的本地单播客机/网关 MAC。
- 将身份保存于容器标签或 Lima 配置，创建与克隆时生成，重新连接、重启和资源修改时复用。
- 同步默认虚拟网卡、DHCP 租约与客机初始化配置，保证改名和 MAC 变化后仍可联网。
- Linux 容器通过 systemd 的 `container_uuid` 接口初始化空白镜像的 machine-id；制作镜像时清理残留的 D-Bus machine-id。
- QEMU 使用新 UUID；Apple Virtualization 的平台身份继续由框架生成。Windows 评估版新安装使用 Setup 随机计算机名。
- 对损坏的身份记录或随机源失败返回错误，保留旧环境兼容与网络隔离检查。

实现说明：`docs/guest-identity.md`。
代码材料：提交时补充 Fork 分支、Commit 链接或本地生成的补丁附件。此草稿尚未上传代码。

### 验证方式与结果

- Linux x86_64，Go 1.26.10，`CGO_ENABLED=0`，使用仓库 Makefile 中的 yq 构建标签。
- `make assets`：Linux 客机代理及模板构建成功。
- 以下 10 个相关 Go 包编译/测试通过：`./internal/containerenv`、`./internal/vm`、`./cmd/cage`，以及 Lima 的 `guestidentity`、`limayaml`、`cidata`、`instance`、`driver/qemu`、`networks/usernet`、`hostagent/dns`（`instance` 包无独立测试文件）。
- 新增回归覆盖 1,000 次身份生成、1,000 次命名样式抽样、完整目录的格式/规模/哈希、拒绝采样、熵源失败、名称大小写兼容，以及创建、克隆、重启配置和资源编辑后的身份稳定。
- 使用相同官方 XML 与主板清单离线重建，CSV 和来源清单完全一致；代表机型简称保留型号/代数，DNS 大小写解析相关测试通过。
- 随机 MAC 下的 ARP、关闭门槛 DNS 与 Ethernet 来源校验通过；旧共享 MAC 不能冒充新环境。
- `bash -n deploy/cloud/build-guest.sh` 与 Windows 应答文件 XML 解析检查通过。
- 真实前端构建完成，构建校验器验证了 20 个资源，命令入口测试使用该构建产物。
- 尚未验证：真实容器镜像构建及首次启动的 machine-id、macOS/Windows 客机实际启动、完整三平台端到端流程。

### 影响范围与已知限制

不自动迁移或重建已有环境。Lima 已有克隆磁盘中的 machine-id、SSH 主机密钥和应用身份不做重置；镜像泛化需要另外处理。Windows 已导入系统不会自动改名。

此方案减少固定身份字段，不保证无法识别虚拟化或容器环境。内核、虚拟硬件型号、软件版本、账号、服务路径、网络拓扑和浏览器指纹仍可能暴露环境特征。CPU/内存配额、时区、语言等保留用户配置含义。

### 补充说明与参考资料

- `docs/guest-identity.md`（提交代码后可补充分支链接）
- [systemd 容器身份接口](https://systemd.io/CONTAINER_INTERFACE/)
- [Windows ComputerName](https://learn.microsoft.com/en-us/windows-hardware/customize/desktop/unattend/microsoft-windows-shell-setup-computername)

按仓库贡献流程，先提交此 Contribution Issue；维护者添加 `accepted` 标签并邀请成为协作者后，再提交指向 `main` 的 PR。在 PR 正文中写 `Closes #实际Issue编号`。

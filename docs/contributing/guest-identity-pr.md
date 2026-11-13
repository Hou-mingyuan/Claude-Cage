# Use realistic PC hostnames and independent guest machine identities

Closes #<ISSUE_NUMBER>

新建环境原先复用固定主机名或 MAC 特征，Lima 默认身份还依赖管理名称和宿主路径。改为在创建和克隆时生成常见 PC 风格主机名、独立 UUID 与默认网卡/网关 MAC，并在后续启动时复用保存的身份。

主机名混合 `DESKTOP-`、`LAPTOP-` 形态和来自 1,593 条公开真实硬件记录的型号简称，附加 6–7 位随机字母数字，最长 15 字符。库内保留 1,181 个不同简称及厂商来源哈希，生成时不联网、不读取宿主硬件；简称用于命名素材，不声称是实际虚拟硬件配置。

容器的默认网卡、虚拟网络 DHCP 和回环主机名保持一致；空白容器镜像通过 `container_uuid` 初始化 machine-id。QEMU 接收新 UUID，Windows 评估版全新安装交由 Setup 生成计算机名。已有环境兼容，身份记录无效时返回错误。

验证：

- Linux x86_64，Go 1.26.10，`CGO_ENABLED=0`，使用仓库 Makefile 中的 yq 构建标签。
- `make assets`：Linux 客机代理及模板构建成功。
- 以下 10 个相关 Go 包编译/测试通过：`./internal/containerenv`、`./internal/vm`、`./cmd/cage`，以及 Lima 的 `guestidentity`、`limayaml`、`cidata`、`instance`、`driver/qemu`、`networks/usernet`、`hostagent/dns`（`instance` 包无独立测试文件）。
- 新增回归覆盖 1,000 次身份生成、1,000 次命名样式抽样、目录格式/规模/哈希、无偏后缀选择、旧名称大小写兼容及生命周期稳定性。
- 型号库使用相同输入离线重建，CSV 与来源清单一致；DNS 大小写解析相关测试通过。
- 随机 MAC 下的 ARP、关闭门槛 DNS 与 Ethernet 来源校验通过；旧共享 MAC 不能冒充新环境。
- `bash -n deploy/cloud/build-guest.sh` 与 Windows 应答文件 XML 解析检查通过。
- 真实前端构建完成，构建校验器验证了 20 个资源，命令入口测试使用该构建产物。
- 尚未验证：真实容器镜像构建及首次启动的 machine-id、macOS/Windows 客机实际启动、完整三平台端到端流程。

限制：本变更不保证环境不可识别，不重置已有克隆磁盘中的 OS/应用身份。Apple Virtualization/Hyper-V 仍各自管理平台标识，三平台真实启动验证范围请以测试记录为准。详细范围见 `docs/guest-identity.md`。

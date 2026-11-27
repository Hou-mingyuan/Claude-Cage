# 从 npm 安装 Cage

安装 Node.js 20 或更新版本后，在终端运行这两条命令。npm 包名为 `claude-cage`，命令名为 `cage`。

```sh
npm install -g claude-cage
cage version
```

首次运行 `cage version` 时，Cage 安装与 npm 包同版本的官方桌面应用，随后输出实际运行的版本。这个入口从 Cage 0.2.2 开始提供。已经安装兼容版本的电脑会直接使用原有 Cage，继续使用原来的账户、项目和环境。使用 `npm install -g --ignore-scripts claude-cage` 也可以在首次运行时完成安装。

输入 `cage` 可查看帮助。`cage login` 打开 Cage 浏览器里的 Claude 登录页。在项目目录运行 `cage claude`，会用默认账号进入 Claude Code。直接提问时用 `cage claude "概括这个项目"`，脚本可以把标准输入传给 `cage claude -p`。旧脚本里如果将整句提示词或 `-p` 紧跟在 `cage` 后面，启动器会在准备原生应用之前提示改写。

用 `cage account list` 核对固定编号、名称和 Claude 邮箱。例如，运行 `cage account rename 03 sarah` 后，`cage sarah -p "概括这个项目"` 会为这次调用使用账号 03。输入的名称找不到时，Cage 会提示查看账号列表。部分早期的 0.2.2 预览版尚未支持按名称选择账号，遇到提示时请在原工作台更新应用。

`cage update --check` 只检查发行信息。已有支持此命令的 Cage 时，由本机程序检查自己的更新通道。尚未安装或仍在使用 0.2.1 时，npm 入口只查询与包版本对应的官方发行信息，显示安装说明。`--check` 与 `--yes` 同时使用仍只做检查。部分早期预览版尚未提供终端更新命令，请按提示从已有工作台升级。

| 平台 | 首次运行时的安装方式 |
| --- | --- |
| macOS，Apple 芯片或 Intel | 校验官方签名并从 DMG 安装。机器上若已有 0.2.1，请先通过原工作台升级。 |
| Windows x64 | 验证官方发布者签名并运行 Cage 安装程序。 |
| Debian 或 Ubuntu x86-64 | APT 校验并安装官方软件包，普通用户通过 sudo 授权。若已有运行中的 Cage，请先保存工作、停止环境并退出工作台。 |

Linux 的 Wayland 程序也可能使用 `cage` 作为命令名。如果已经安装它，请使用 `claude-cage`。将 npm 的全局命令放在自己的 npm 前缀或系统的本地程序目录，可以保留发行版管理的文件。

`npm uninstall -g claude-cage` 移除 npm 入口。原生应用仍可通过系统的应用管理方式卸载。

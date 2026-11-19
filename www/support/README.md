# 官网使用答疑

访客从首页或使用手册进入 `/ask/`。对话历史和截图保存在访客浏览器的 IndexedDB 中，可从侧栏重新打开或删除。每次追问最多提交最近六轮文字作为上下文。勾选联网时，服务会用当前问题检索 Microsoft Learn 和中文维基百科。

## 更新规则

生产服务每次接受提问时读取 `/opt/cage-support/rules.md`。可以在本机编辑 Markdown，然后用以下命令应用。临时文件和目标文件位于同一目录，替换完成后下一次提问就会使用新规则。

```sh
ssh token-link-server 'set -eu; tmp=$(mktemp /opt/cage-support/rules.XXXXXX); cat > "$tmp"; chmod 644 "$tmp"; mv "$tmp" /opt/cage-support/rules.md' < 官网答疑规则.md
```

## 更新程序和知识库

在仓库根目录执行以下命令。将生成的程序、语料及其提交标识同步到服务器的 `/opt/cage-support/`，再重启 `cage-support.service`。语料取自指定提交，包含使用手册和应用源码。源码引用指向该提交的具体行。访客询问实现细节时，服务会将产品用语转换为代码检索词，优先匹配问题中点名的函数或配置项，并读取相邻代码段，以覆盖跨段的分支和返回值。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/server ./www/support
bash www/support/build-corpus.sh origin/main /tmp/corpus.tar.gz
```

首次安装时创建空的 `/opt/cage-support/rules.md`，并将 `TOKEN_LINK_API_KEY` 写入仅 root 可读的 `/etc/cage-support/model.env`。服务调用 `api.token-link.shop` 的 `deepseek-flash`。将同目录的 systemd 单元安装到 `/etc/systemd/system/`。

官网发送提问或确认发布 Issue 时，在正式页面取得专用的 Turnstile 票据。答疑服务通过本机 18156 端口的验证服务核对域名、操作和票据使用状态，通过后才执行请求。浏览器仅能读取公开的站点配置。外部代理只向公众开放答疑和客服页面路由，验证服务的 `/local/` 路径保留给本机进程。

HTTPS 入口 `https://token-link.shop/cage-support/ask` 将该路径改写为 `/ask` 并转发至 `127.0.0.1:18152`。代理必须用连接对端 IP 覆盖 `X-Real-IP`。服务每个来源 IP 每小时接受 20 次请求，全部访客每天合计 600 次，同时处理 5 个问题。计数以 UTC 整点和自然日为界，服务重启会重置计数。

## 通过飞书补充答案

访客询问尚未确定的商务条款、服务承诺或未来计划时，助手先复查现行规则与代码资料。原问题和最终复核都指向同一类待定政策时，官网才保存通用问题摘要，由机器人私发给维护者。实现和兼容性问题留在访客对话里，助手可沿模块说明与函数继续检索，并说明需要实测的部分。访客需要补充版本或错误原文时，助手会在网页继续追问。

只有一条已送达的待答问题时，可以紧接机器人消息直接回复。服务会用模型核对这条私信是否明确回答该问题，再保存你的原话。多条问题同时待答时，在飞书中引用机器人的原问题，回复可直接公开的文字答案，或单独发送 `答 问题编号 答案`。服务器每 30 秒检查一次该私聊，保存维护者答复后，同类提问即可引用。再次引用原问题可以更正答案，引用后回复“忽略”可以跳过问题或撤下已有答案。

服务使用独立的 Linux 用户 `cage-support`，其主目录为 `/var/lib/cage-support`。用这个用户配置飞书 CLI 的专用机器人，安装 CLI 到 `/opt/cage-support/lark-cli`。在 `/etc/cage-support/feishu.env` 中设置 `SUPPORT_FEISHU_OWNER=ou_…`，取值是维护者在该应用下的 open_id。机器人需要向该用户单聊发消息和读取这段单聊历史的权限。回复按应用身份、私聊、发言人和原问题匹配。

问题、答复和读取进度保存在 `/var/lib/cage-support/knowledge/`。更新服务时保留此目录。暂时断网时问题留在队列，连接恢复后继续投递并补读回复。


## 图片、诊断日志、文档与 Issue

访客可上传、粘贴或拖入一张 PNG、JPEG 或 WebP 图片。浏览器会缩小并重新编码，服务使用 deepseek-flash 提取可见事实，再按问题和截图内容检索。手册引用可在对话中预览，完整文档链接使用网站构建时生成的章节锚点。

访客可上传或拖入不超过 96 MiB 的 ZIP 日志包。浏览器本地读取未加密的文本日志、JSON、JSONL 等文件，按版本清单、当前日志和报错选择节选，显示读取范围。访客可检查、编辑后发送。对话历史保存节选，原 ZIP 无需上传。

访客要求反馈时，助手根据已知信息整理草稿。访客编辑并确认后，项目机器人向 Lee-xeo/Claude-Cage 创建公开 Issue，页面显示编号和链接。待确认草稿 24 小时内有效。每个来源 IP 每个 UTC 自然日最多代发 3 条，全站合计 25 条。服务使用本地提交记录处理重复点击和结果不明的重试。

GitHub App 只需安装到上述仓库，配置 Issues 读写权限。将含数值字段 `id` 和 PEM 字符串字段 `pem` 的 `github-app.json` 放在服务的工作目录，由 root 拥有并允许服务组读取（0640）。短期安装令牌只申请此仓库的 Issues 权限。提交记录保存在知识库同级的 `issue-drafts/`。

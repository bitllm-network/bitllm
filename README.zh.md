# BitLLM_

[![test](https://github.com/bitllm-org/bitllm/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/bitllm-org/bitllm/actions/workflows/test.yml)
[![release](https://img.shields.io/github/v/release/bitllm-org/bitllm)](https://github.com/bitllm-org/bitllm/releases)
[![MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

[English](README.md) | 中文

> 用 AI agent 帮你装？把 https://bitllm.org/llms.txt 发给它。要改代码？看 [AGENTS.md](AGENTS.md)。

**本地大模型的 BitTorrent。** 分享你自己的模型，也能用别人分享的。

每个节点跑同一个二进制。它可以把你已经在跑的 OpenAI 兼容服务（vLLM、SGLang、llama.cpp、Ollama、LM Studio、MLX 等）分享出去，同时在本机提供一个网关：你请求哪个模型，它就找哪个在线的节点回答。没有协调者：节点通过 Kademlia DHT 和 gossip 互相发现，能直连就直连，两边都在 NAT 后面时就走中继加打洞。

内测阶段，MIT 许可。网络已经在线：[bitllm.org](https://bitllm.org)。邀请决定优先级，不决定能不能加入：[访问规则](#访问规则)决定谁先用上名额，没被邀请的节点也能用空闲名额。使用不需要账号，也不需要钱包。

![节点运行时的控制台：终端风格的编辑器、模型列表、可复制的配置和测试框](docs/images/dashboard.png)

## 网络如何运作

四个角色，同一个二进制：

```
   你的节点                  公网中继 / seed               GPU 主机
  127.0.0.1:7778           DHT、中继、收据              vLLM / Ollama
        │                        ▲                         ▲
        │  首次接触              │                         │
        └────────────────────────┘                         │
        │                                                  │
        └──────── 直连，或中继加打洞 ─────────────────────┘
```

- **你的节点**和其他人跑同一个二进制。网关监听 `127.0.0.1:7778`，同时支持 OpenAI 和 Anthropic 格式，agent 把它当成普通 API 用就行。默认情况下，互联网上的机器连不到这个网关。
- **服务节点**就是你的节点加 `--upstream`，指向你本机已经在跑的模型服务。它可以躲在 NAT 后面：其他节点通过中继加打洞找到它，不需要做端口转发。
- **公网中继**是有入站 TCP/UDP 4001 的服务节点，加上 `--public`。它必须服务一个模型，因为只有服务节点才会 announce。它回答 DHT 查询、给 NAT 节点做中继，但仍然从默认 seed 加入，所以不需要 DNS 记录。它的 announce 带 `relay=true`，autorelay 会把它列为候选；NAT 节点会在 seed 和公网中继上都保留位置，而不是全挤在 seed 上。
- **seed** 既是公网中继，又列在 `_dnsaddr.bootstrap.bitllm.org` 里，并且用 `--dht-server` 独立启动。它是新节点的首次接触点。一个 seed 就够加入；公网中继让中继容量随网络增长。

发现分两步。二进制先解析 `_dnsaddr.bootstrap.bitllm.org` 拿到 seed 地址，之后模型在哪个节点上、怎么连过去、哪些公网节点能做中继，都由 DHT、gossip 和 relay announce 负责。域名解析不到时，它会再试 `boot.bitllm.org` 后备；两个都不通，节点会自己起一个网络，并打印 join code，另一台机器用 `--bootstrap` 接上就行。

只有下面这些部分是集中托管的，而且都不在推理链路上：

| 部分 | 放在哪 | 做什么 | 挂了会怎样 |
|---|---|---|---|
| DNS 种子列表 | Cloudflare DNS，`_dnsaddr.bootstrap.bitllm.org` | 首次接触时告诉节点有哪些 seed | 有 join code 的节点仍能加入；`boot` 只能补一条缺失的记录，救不了整个 zone |
| 网站和安装脚本 | Cloudflare Worker，bitllm.org | 页面、`install.sh`、`llms.txt` | 已安装的节点照常运行；可以从 GitHub 手动安装 |
| 发布二进制 | GitHub | 下载、校验和、自动更新 | 新安装和更新暂停 |

其余全是点对点：没有协调者、没有数据库、没有 KV；地图是从 announce 算出来的快照。Cloudflare 只提供 DNS 和静态网站，不转发推理；唯一的例外是通过过渡期 `boot` 隧道中继的流量。DNS 只用来找第一个节点；推理请求从你的网关直接到服务节点，或者经过中继。公网中继不是集中托管的：任何有公网地址的运营者都能跑一台。现在跑 `boot.bitllm.org` 的 Mac mini 只是过渡期的后备，不属于设计的一部分。

### 每个部分依赖什么

| 部分 | 需要 | 不需要 |
|---|---|---|
| 节点和网关 | 出站网络；一个 seed 或 join code 来认识其他节点 | 入站端口、账号、钱包 |
| 服务节点 | 本机已经跑着的 OpenAI 兼容服务 | 公网 IP、端口转发、seed 角色 |
| 公网中继 | 一个公网地址；入站 TCP 和 UDP 4001 | DNS 记录、GPU、模型 |
| seed | 一个列在 DNS 里的公网中继；一直开着 | GPU、模型、数据库 |
| DNS 种子列表 | bitllm.org 的 DNS zone（这里是 Cloudflare）和 TXT 记录 | Worker 或任何运行时 |
| 网站和安装脚本 | 一个带静态资源的 Cloudflare Worker | KV、数据库、Mac mini |
| 自动更新 | GitHub Releases 和 `SHA256SUMS` | 包管理器 |

seed 故意做得小而独立：一台在训练、渲染或者干脆关机的 GPU，不能把首次接触也一起带走。有入站的 GPU 主机仍然可以用 `PUBLIC=1` 做公网中继，增加中继容量而不增加 DNS 记录。新节点有一个 seed 就能加入；第二台公网中继（不管在不在 DNS 里）能在 seed 忙时继续接住流量。模型细节见 [docs/bootstrap.md](docs/bootstrap.md)，部署清单见 [docs/seed-handoff.md](docs/seed-handoff.md)。

目录：[网络如何运作](#网络如何运作) · [安装](#安装并运行) · [分享模型](#分享模型) ·
[使用网络](#使用网络) · [管理节点](#观察和管理节点) · [积分](#服务积分) ·
[访问规则](#访问规则) · [seed](#运行-seed) · [注意事项](#需要知道的事) ·
[构建](#构建和测试) · [贡献](#参与贡献)

## 安装并运行

不用端口转发，也不用配置。安装脚本会自动选对版本、核对校验和，装到 `~/.local/bin`（不需要 sudo）。节点通过 DNS 种子列表加入网络，见 [docs/bootstrap.md](docs/bootstrap.md)；在过渡期，解析不到时还会试 `boot.bitllm.org`。两个都不通时，它会自己起一个网络并打印 join code：`bitllm --bootstrap /ip4/.../p2p/...`。

```bash
# macOS（Apple 芯片）、Linux（x86-64、arm64）
curl -fsSL https://bitllm.org/install.sh | sh
~/.local/bin/bitllm
```

```powershell
# Windows（PowerShell）：装到 %LOCALAPPDATA%\bitllm
irm https://bitllm.org/install.ps1 | iex
& "$env:LOCALAPPDATA\bitllm\bitllm.exe"
```

下面的例子都写 `bitllm`；如果 `~/.local/bin` 不在 PATH 里，就用完整路径（安装脚本会告诉你；Windows 上是 `%LOCALAPPDATA%\bitllm`）。想手动下载？到 [Releases](https://github.com/bitllm-org/bitllm/releases) 拿对应系统的文件；macOS 上用 curl 下载，避免浏览器打上隔离标记。

节点在前台运行。然后打开 http://127.0.0.1:7778，选一个模型，复制给你的 agent 的配置，发一条测试消息。想关了终端还继续跑，见[让它一直运行](#让它一直运行)。

运行时，另开一个终端执行 `bitllm top` 可以看到节点、交换的活和日志；见[观察和管理节点](#观察和管理节点)。

## 分享模型

你需要一个在本机跑着的 OpenAI 兼容服务。把节点指过去：

```bash
bitllm --upstream http://localhost:8000/v1 --concurrency 2
```

- 不加 `--model` 时，你的服务列出什么模型，就按它报告的那个 id 原样分享。Hugging Face 的 id（比如 `Qwen/Qwen3.8-Flash-Next`）会保留斜杠和大小写。节点运行时会重新读取模型列表，所以之后在 vLLM 里新加的模型不用重启 BitLLM 就会出现。
- `--model 本地=公开`：只有本地服务叫的名字和网络名字不一样时才需要。等号左边是你的服务叫的名字，右边是网络名字。
- `--concurrency`：同时接几个请求。多出来的请求在这个节点的队列里等，先按访问等级、再按到达顺序；等得越久等级每 20 秒升一级，但不会超过最高等级，之后到达顺序决定，所以同级里等得最久的先走。队列满了立刻回答，不让人等一个不存在的名额。
- `--max-tokens`（默认 32768）：一个请求最多要多少新 token。每个请求只保留一个选择（`n` 会被丢掉）。
- 来自其他节点的请求会被收窄：节点只转发已知字段，图片只收 `data:`，每个 `/completions` 请求只有一个 prompt。`--peer-field NAME` 可以加字段。你自己发给自己服务的请求不受影响。
- 图片：如果你的服务限制了每个 prompt 的图片数（vLLM 的 `--limit-mm-per-prompt`），节点会从服务第一次拒绝里学到这个上限，之后只发最新的图片，旧的用一句短说明代替。Agent 每一轮都会把整个会话的截图再发一遍，这样长会话才不会撞上限。不用设置；改上限后重启节点。
- `--location LAT,LON`（可选）：声明一个粗略位置，给 bitllm.org 的地图用。填城市里任意一点，比如 `--location 52.52,13.40`；节点只广播大约 150 公里的格子，永远不发这个点。不加就记为"没有位置"。细节见 [docs/network-map.md](docs/network-map.md)。

### 更多参数

`bitllm --help` 列出所有参数。下面这些不太常用：

- `--upstream-key KEY`：模型服务的 API key。也可以用 `BITLLM_UPSTREAM_KEY`，这样不会出现在进程列表里。
- `--upstream-timeout DURATION`：节点为一个请求等你的模型服务多久。默认 10 分钟。
- `--peer-field NAME`：多转发一个来自其他节点的请求字段。可重复。
- `--peer-fallback`：你自己的服务挂了时，用其他节点回答你分享的模型。那些节点能读到 prompt。
- `--open-slots N`：配合 `--open`，没被邀请的节点最多同时占几个名额。默认是 `--concurrency` 的一半，至少 1 个。
- `--open-daily N`：配合 `--open`，所有没被邀请的节点每个 UTC 日合计能用多少积分，用完后只能等空闲名额。默认 0，不限。
- `--audit-every N`：配合 `--audit`，给一个节点发多少个真实请求后抽查一次。默认 50。
- `--audit-serve`：如果你的服务返回 prompt logprobs（vLLM），就替其他节点的 `--audit` 打分。
- `--gateway ADDR`：网关地址。默认 `127.0.0.1:7778`。
- `--gateway-key KEY`：客户端要当作 API key 发送的密钥。也可以用 `BITLLM_GATEWAY_KEY`。网关在非 loopback 地址上时需要它；没设的话节点会警告，网关是开放的。
- `--mdns`：在本地网络里找节点。默认开，`--mdns=false` 关闭。
- `--roots ADDR`：邀请树的根账户。可重复。默认是 BitLLM 的根账户。用于私有网络和测试。
- `bitllm invite --revoke 0xADDRESS`：撤销一个账户。只有根账户能用。

### 模型命名

网络名字就是客户端请求里的 `model` 字符串。两台机器广播同一个字符串，对这个模型来说就可以互换：网关挑一台，它不答了再换另一台。所以名字要说明权重，而不是说明机器。

1. **公开模型用 Hugging Face 的原 id。** `Qwen/Qwen3.8-Flash-Next`，斜杠和大小写都照发布的样子。独立的主机只要跑同一个仓库就能对上，不需要中心登记；一台下线，另一台可以接过去。
2. **微调或量化用实际权重的仓库 id**，有的话；没有就在基础模型后面加后缀说明改了什么。后缀直接接在 id 后面，例如 `Qwen/Qwen3.8-Flash-Next-fp4-alesha`。
3. **私有或全新的模型可以随便取一个稳定的名字。** 不需要先登记：节点会自动广播，`GET /v1/models` 会列出来，dashboard 里可以直接选。只有本地服务报告的 id 不是客户端该用的名字时，才需要 `--model`。

下面的表是 beta 到目前为止给参考模型用的短名字，不是门槛；任何 id 都能用。

| 网络名字 | 权重 |
|---|---|
| `deepseek-v4.1-flash-1017` | DeepSeek V4.1 Flash，1017 版，官方权重 |
| `deepseek-v4.1-flash-1017-heretic` | 同上，heretic（去审查）微调 |
| `qwen3.8-flash-next-fp4-alesha` | Qwen3.8 Flash-Next，NVIDIA FP4-Local 量化，alesha 去审查微调 |
| `qwen3.8-flash-next-nvfp4-lil` | Qwen3.8 Flash-Next，官方权重，local-inference-lab 的 NVFP4-MXFP8-CSF-QAD 量化（用 NVIDIA Flare 跑：固定采样，temperature 1.0 / top-p 0.95 / top-k 20） |

同一个名字必须对应同一套权重。用 Hub id 是最简单的做法。

**名字怎么拼：** `<模型>[-<量化>][-<来源>]`，用连字符连接；模型部分保留 Hugging Face 仓库 id 的大小写和斜杠。

| 部分 | 表示什么 | 什么时候省略 | 例子 |
|---|---|---|---|
| 模型 | 基础模型和版本，用厂商的名字 | 不能省 | `Qwen/Qwen3.8-Flash-Next`、`deepseek-v4.1-flash-1017` |
| 量化 | 权重格式，不是厂商原版时 | 厂商发布的原版权重 | `fp4`、`nvfp4`、`int4`、`q4_k_m` |
| 来源 | 谁改了权重：微调名，或者做量化的团队 | 厂商发布的原版权重 | `heretic`、`alesha`、`lil` |

服务怎么跑（vLLM 还是 SGLang、上下文长度、固定采样）不改变名字；写在表里备注。要加参考名字时开一个 issue，写清：基础模型、权重来源（链接）、量化格式和谁做的、客户端需要知道的事（固定采样、上下文长度、图片支持）。不需要提供服务器地址：节点会在网络里互相找到。

## 使用网络

每个节点，分不分享都一样，都有一个网关 `http://127.0.0.1:7778/v1`：

```bash
bitllm                                 # 只用，不分享
curl localhost:7778/v1/models
curl localhost:7778/v1/chat/completions \
  -d '{"model":"deepseek-v4.1-flash-1017","stream":true,"messages":[{"role":"user","content":"你好"}]}'
curl localhost:7778/status             # 节点 ID、连接、账本
curl localhost:7778/queue              # 名额、正在跑的活和等待队列
```

用网络不需要账号。`/v1/models` 列出此刻在线的模型，以及每个模型有几个节点在服务。[网络地图](docs/network-map.md)上的名额总数按模型计算：一个节点分享几个模型就被算几次，所以把各模型加起来会高估容量。

打开 `http://127.0.0.1:7778` 有控制台：给你的 agent 的现成配置、连接测试。网关同时说 OpenAI（`/v1/chat/completions`、`/v1/completions`）和 Anthropic（`/v1/messages`），Claude Code 直接能用：

```bash
M=deepseek-v4.1-flash-1017
ANTHROPIC_BASE_URL=http://127.0.0.1:7778 ANTHROPIC_API_KEY=bitllm ANTHROPIC_MODEL=$M \
ANTHROPIC_DEFAULT_OPUS_MODEL=$M ANTHROPIC_DEFAULT_SONNET_MODEL=$M ANTHROPIC_DEFAULT_HAIKU_MODEL=$M \
CLAUDE_CODE_SUBAGENT_MODEL=$M claude
```

OpenAI 兼容客户端（Cline、Continue、OpenCode、Aider 等）：Base URL 填 `http://127.0.0.1:7778/v1`，API key 随便填。Anthropic 请求要求服务方的上游支持 `/v1/messages`（vLLM 支持）。响应头 `X-Bitllm-Peer` 告诉你哪个节点回答的。

你分享的模型留在你的机器上：网关用你自己的服务回答，免费。你的服务挂了时，网关返回 503。加 `--peer-fallback` 才会改用其他节点，那些节点能读到 prompt。

服务方对来自其他节点的请求做了收窄：只收已知字段，图片只收 `data:`，每个 `/completions` 请求只有一个 prompt。如果客户端发了不常见的字段，服务方可能要用 `--peer-field` 放行。

## 观察和管理节点

`bitllm top` 是终端里的实时视图：你连着谁、分享了什么、和每个节点交换了多少活、日志的最后几行，每秒刷新。

```bash
bitllm top                     # 看 127.0.0.1:7778
bitllm top --interval 2s       # 慢一点刷新
bitllm top --gateway 127.0.0.1:9000
q                               # 退出；节点继续跑
```

它只通过 HTTP 读本机网关，所以随便节点怎么启动都能看：另一个终端、tmux、systemd 都行，而且不碰节点的状态。节点停了它会显示原因并一直重试，你可以开着它重启节点。在管道里（`bitllm top | cat`）它打印一帧就退出，适合 `ssh` 或写日志。

同样的数据不用终端也能拿：

```bash
curl localhost:7778/status        # 节点 ID、地址、连接、服务状态、运行时长、账本
curl localhost:7778/logs?n=50     # 节点留在内存里的最后 50 行日志
curl localhost:7778/credits       # 每个服务节点的积分
```

节点在内存里保留最后 500 行日志给 `/logs`，同时照常写 stderr，所以日志文件、`journalctl`、终端里的 `tee` 都不受影响。

### 让它一直运行

想有个能 detach 再回来的终端，用 tmux：

```bash
tmux new -s bitllm 'bitllm 2>&1 | tee -a ~/.local/state/bitllm.log'
# ctrl-b d 离开，节点继续跑；tmux attach -t bitllm 回来
```

Linux 上，用用户服务，登录时启动、停了自动重启：

```ini
# ~/.config/systemd/user/bitllm.service
[Unit]
Description=BitLLM node
After=network-online.target
[Service]
ExecStart=%h/.local/bin/bitllm
Restart=always
RestartSec=5
[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload && systemctl --user enable --now bitllm
loginctl enable-linger "$USER"      # 退出登录后继续跑
journalctl --user -u bitllm -f      # 日志
```

如果节点还要分享模型，把分享参数加到 `ExecStart`。macOS 上，`~/Library/LaunchAgents` 里带 `KeepAlive` 的 LaunchAgent 做同样的事。

### 更新

发布版启动 30 秒后检查一次新版本，之后每 6 小时一次。有新版就下载对应机器的二进制，核对 release 的 `SHA256SUMS`，跑一次确认它报告的版本，然后替换。如果节点正在服务，会等到没有任务在跑、队列也空了，再原地重启：同一个终端、同一个 systemd 单元、同一个节点 ID。最多等 24 小时，到时间就直接重启，免得一个卡住的客户端永远挡住更新。等待期间 `GET /status` 显示 `pending_restart: true`。Windows 上运行中的 `.exe` 换不掉，所以新二进制装好，下次启动时生效。

HTTPS 和校验和能挡住损坏或截断的下载。每个 release 还会发布 `SHA256SUMS.sig`，这是对 tag 和校验和文件的签名，用管理员离线保管的密钥生成，并用内置在二进制里的公钥核对。签名缺失或不对的 release 会被拒绝：不下载，正在运行的二进制保持原样。`bitllm update` 会打印结果，`GET /status` 里的 `signature` 显示 `ok`、`missing`、`bad` 或 `no-keys`。

`--auto-update=false` 留在当前版本。`bitllm update` 立刻做同样的检查，也是 supervisor 下更新节点的方式。`--check` 只报告，签名不是 `ok` 时以非零状态退出。`bitllm update --tag vX.Y.Z` 用同样的检查安装指定的那个 release，可以用来试 prerelease，因为常规检查会跳过它们。它也能退回旧版本，但开着自动更新运行的节点会再次往前更新，除非用 `--auto-update=false` 启动。被替换的二进制留在旁边叫 `bitllm.prev`，`mv bitllm.prev bitllm` 就回退一个版本。源码构建（`dev`）和 CI 构建不会自更新，在它们上面跑 `bitllm update` 同样要求签名。`bitllm --version` 打印当前版本，`GET /status` 显示上次检查。更新器要写二进制所在的目录，所以包管理器安装或者只读挂载需要 `--auto-update=false`；否则节点会记一条错误然后继续跑。

节点自己装上的新版本要先证明能用：在它带着 gateway 连续运行满 60 秒之前，每次启动都会计数。三次启动失败后，下一次启动会把 `bitllm.prev` 换回来并重启进去。那个版本不会再被安装，更新的版本照常安装。这次启动之后 `GET /status` 显示 `rolled_back_from`，失败的二进制留在旁边叫 `bitllm.failed`。这要靠有东西重新启动节点，比如上面的 systemd 单元；在普通终端里，只有你再次运行时才会计数。用 `--auto-update=false` 启动的节点不会自己换二进制。

### 文件都在哪

节点存的东西都在 `~/.bitllm`（`--data` 可以改）：节点密钥、签收据用的种子、收据日志、账本、会员和额度状态。删掉这个目录就用新身份重新开始。

## 服务积分

你每服务一个任务就得到积分（prompt token / 4 + completion token），记在一张收据里：客户端签收它收到的东西（模型、token 数、响应字节的 SHA-256），你的节点核对后副签，收据通过 gossip 广播到全网。

收据是双方自己上报的。双方都签名，但两把密钥可能属于同一个人，所以收据不能证明活真的干过。

客户端对每个服务端用一把密钥，每 30 天更换一次。这个服务端能把你的任务互相关联起来，其他节点没法跨服务端关联到你。收据显示哪个服务端干了活，不显示谁问的。

积分必须等于 token 公式，每个节点每分钟最多接受每个服务端 120 张收据（总共 600 张）。任何节点都能验证两个签名，并用它听到的收据算出积分，所以不同节点的总数可能不一样：

```bash
curl localhost:7778/credits      # 每个服务节点的积分，来自这个节点见过的所有收据
```

收据存在 `~/.bitllm/receipts.jsonl`；新节点从它的 seed 节点补历史。积分以后有什么用还没定。定之前会先公布哪些收据算数：自己开小号刷自己、或者服务一个和名字不符的模型，都不算。

## 访问规则

邀请决定优先级，不决定能不能加入。三条规则：

1. 有邀请，任何空闲名额都能用。没被邀请的节点，只在服务方空闲时才会被服务，除非服务方选择对所有人开放。
2. 默认没有每日额度限制，服务方可以自己设置。
3. 分享算力就是贡献者：节点服务过任务后，邀请名额从 3 个变成 10 个；最近 7 天内让你的节点干过活的服务方，不会用额度限制你。

还不是会员也能用网络：服务方空闲时就会服务你，用 `--open` 启动的服务方会像对待会员一样对待你。想成为会员，找一位会员邀请你的以太坊地址，然后在控制台把节点关联到这个地址（"Link wallet" 或 `bitllm account link`）。关联是公开的，见[账户](#账户)。用网络不需要钱包，只有想要邀请或额度时才需要一个地址。

会员 = 根账户，或者被会员邀请的账户。你的节点服务过一次任务，就永久有 10 个邀请名额。这个上限读的是公开收据，而收据是自己上报的。服务方可以给每个会员设每日额度（`--free-daily`）。额度豁免不看公开收据，只看服务方自己见过的事：最近 7 天内，你的节点完成过它发出的任务。从不向其他节点发请求的服务方不会豁免任何人。

根账户可以撤销一个账户（`bitllm invite --revoke`）。这个账户和它邀请进来的人都会退回未邀请这一档，仍然能用空闲名额。它邀请进来的人之后可以被别人重新邀请。

邀请由以太坊账户签名后广播，所以没有会员服务器。会员名单随着签名内容的传播逐渐一致，不同节点可能暂时看法不同。服务方通过节点的 claim 查请求方的账户（见下面）。

```bash
bitllm invite 0xADDRESS           # 用助记词账户邀请
```

用浏览器钱包的话，在控制台的 Invite 框里操作。

### 只服务指定的人（--allow）

只想分享给几个人，就列出他们的节点 ID（每个节点启动时会打印自己的 ID，`curl -s 127.0.0.1:7778/status` 里的 `id` 也是）：

```bash
bitllm --upstream http://localhost:8000/v1 --model ... \
  --allow 12D3KooW...friend1 --allow 12D3KooW...friend2
```

- 名单里的节点不管有没有被邀请，都按会员对待，任何空闲名额都能用；`--free-daily` 照样生效。
- 其他节点一律拒绝，包括会员，错误是 `403 not allowed: this server serves only the peers its owner listed with --allow. Your node is 12D3KooW...; send that id to the owner to be added`。对方的网关会跳过你、同时去找别的服务方；如果只有你在分享这个模型，它会立刻收到这句话，不用等。
- 你的模型仍然出现在所有人的 `/v1/models` 里，因为列表不是广播的。
- 不加 `--allow` 一切照旧，按上面三条规则。
- 名单只在启动时读取；改了要重启节点。

### 账户

积分记在节点上。想把自己几台机器的积分合到一个账户下，就把每个节点关联到同一个以太坊地址。只是一个签名（personal_sign）：不发交易、不花 gas、不用 RPC、不上链。Claim 由钱包和节点密钥双方签名，所以没人能认领你的节点；每个节点以最新的 claim 为准。

**关联是公开的。** 它会把这个节点的 ID 连同你的钱包地址和显示名字发布给网络上的每个节点。先想好要不要这样，再执行下面的步骤：

```bash
bitllm account new --name alice     # 新的 12 词账户，并关联这台机器
bitllm account link --name alice    # 把另一台机器关联到同一个账户
bitllm account                      # 看这台机器归谁
```

或者在控制台点 "Link wallet" 用浏览器钱包（MetaMask、Rabby 等）签名。智能合约钱包暂时不支持（它们的签名需要链上验证）。

另外，每个节点保留一份和每个节点交换的私有账本（`~/.bitllm/ledger.json`），只是数据。

## 运行 seed

seed 是新节点第一个连的节点。它回答 DHT 查询，给 NAT 后面的节点做中继直到两边打洞直连，并把收据历史提供给刚加入的节点同步。它是一台列在 `_dnsaddr` 里的公网中继；不回答推理请求，也不决定会员资格。

第一台 seed 要和 GPU 分开。一台一直开着的便宜 VPS，或者任何有公网地址的机器，是常见选择；这样一台在训练、渲染或者关机的 GPU 不会把首次接触也一起带走。新节点有一个 seed 就能加入。

```bash
bitllm --dht-server --port 4001 \
  --announce /ip4/203.0.113.10/tcp/4001 \
  --announce /ip4/203.0.113.10/udp/4001/quic-v1
```

有公网地址、但不需要做“第一个节点”的机器，跑成**公网中继**：

```bash
bitllm --public --port 4001 \
  --upstream http://localhost:8000/v1 \
  --announce /ip4/203.0.113.11/tcp/4001 \
  --announce /ip4/203.0.113.11/udp/4001/quic-v1
```

它从默认 seed 加入，回答 DHT 查询、给 NAT 节点做中继，但不需要 `_dnsaddr` 记录。它必须服务一个模型，因为只有服务节点才会 announce。它的 announce 带 `relay=true`，NAT 节点会把它当中继候选；中继容量就这样随公网节点增长。`deploy/gpu-host.sh` 加 `PUBLIC=1` 就是这种装法。

开放 TCP 和 UDP 4001。在一台全新的 Ubuntu VM 上，`deploy/bootstrap-vps.sh` 会装好 systemd 单元并打印要发布的 DNS 记录。家里的机器也能做公网中继，用端口转发或者 UPnP；但大多数家用机器只是普通的服务节点：通过 seed 加入，别人通过中继或者打洞找到它。

`--dht-server` 和 `--public` 都会强制自己对外可达，所以只有入站真的通了才开。否则节点会广播别人连不上的地址。只有第一台 seed 用 `--dht-server`，因为它要在自己即将发布的 DNS 列表还不存在时先启动；之后所有公网主机都用 `--public`，它会从默认 seed 加入。第二台 seed 要指向一个活着的：
`bitllm --dht-server --bootstrap /dnsaddr/bootstrap.bitllm.org`。

[docs/host-a-gpu.md](docs/host-a-gpu.md) 是一台要分享模型的机器的逐步清单，包括判断它能不能同时做公网中继的检查。`deploy/gpu-host.sh` 把服务装在模型服务旁边。

默认列表是 `_dnsaddr.bootstrap.bitllm.org` 的 DNS 种子列表。用 `deploy/seed-dns.py` 发布，新节点通过其中任意一条记录加入。加或撤 seed 是改 DNS，不是发版。DNS 列表和过渡期的 `boot.bitllm.org` 后备都连不上时，节点会自己起一个网络并打印 join code，不会失败。失败行为见 [docs/bootstrap.md](docs/bootstrap.md)，部署清单见 [docs/seed-handoff.md](docs/seed-handoff.md)。

没有公网 IP？把它放到一个能过 WebSocket 的 HTTPS 隧道后面（Cloudflare Tunnel，免费）：
`bitllm --dht-server --ws-port 4002 --announce /dns4/boot.example.org/tcp/443/wss`，隧道指向 `http://127.0.0.1:4002`。别人用
`--bootstrap /dns4/boot.example.org/tcp/443/wss/p2p/<节点 ID>` 加入。这样 Cloudflare 在连接链路上，所以只当后备，不要当主 seed。

要重启先通知。节点每 15 秒重连一次丢失的 seed，但已经在通过你的中继传输的请求会被切断，用户的 agent 不一定重试。

## 需要知道的事

- **帮你推理的节点能看到你的 prompt**，和任何推理服务商一样。传输是加密的（Noise/TLS/QUIC），但服务方机器要读明文才能推理。不要发敏感信息。
- **和你直连的节点能看到你的 IP**，服务你的节点能看到你的节点 ID。在意的话，通过 VPN 或者能走 Tor 的 VPS 使用。
- 你分享模型时，你服务的客户端也能看到你的 IP。
- **节点可能用别的模型冒充。** 启动时加 `--audit`（默认关）抽查回答你的节点：大约每 50 个请求给某个节点发一个探针，另一个节点（审计方）逐 token 打分；一直不合格的节点 24 小时内不再接这个模型的请求（`GET /audits` 看结果）。默认审计方是我们（维护者）跑的一个节点；`--auditor PEER_ID` 换掉它，`--auditor none` 关掉。这只是抽查，不是证明：
  - 能查出跑的是另一个模型，以及贪心请求下换成了 Q4 或更粗的量化。
  - 查不出能认出探针的节点（探针 prompt 在源码里公开）、只在采样请求上换量化、或者把官方权重换成 heretic 这种轻度微调。
  - 审计方可以说谎：放过作弊者，或者给诚实的节点编坏分数。打乱的控制组只能抓住全放过的审计方。信任 seed 就是信任我们。
  - 同一个人的服务节点和审计方可以互相放过。同一个 owner 或者同一个 /24 的节点不会被配对，但不同网络上的未认领节点可以。
  - `--weights NAME=HASH` 只是服务方说自己加载了什么。节点只是转发它的服务，证明不了。
  数字和细节：[docs/audit.md](docs/audit.md)。
- `--upnp`（默认开）会让路由器转发 p2p 端口。
- **IPv6 会把 p2p 端口暴露在公网上。** 机器有公网 IPv6、路由器又放行入站 IPv6 时，互联网上任何人都能连到 4001 端口（只是 p2p 端口；7778 的网关默认只监听 127.0.0.1）。这本来是为了让别人能直连你，但如果你不想在这台机器上开放端口，启动时加 `--ipv6=false`，只在 IPv4 上监听。代价：只有 IPv6 的节点没法直连你，更多流量要走中继，可能慢一些；IPv4 上一切照旧。

## 构建和测试

Go 1.26 或更新（`mise.toml` 固定工具链）。没有 C 依赖，一个二进制；包按领域放在 `cmd/` 和 `internal/` 下。

```bash
go build -o bitllm ./cmd/bitllm
go vet ./...
go test -race ./...              # 127.0.0.1 上真实的 libp2p 节点；30 到 60 秒
```

四个平台的发布二进制和校验和由 `scripts/release.sh vX.Y.Z` 生成。测试覆盖发现、纯中继路径、服务队列、收据、账本和访问规则。目录结构和容易踩的规则见 [AGENTS.md](AGENTS.md)。

协议：`/bitllm/infer/1.1.0`（一行 JSON 请求，等待名额时零或多行 `202` 位置，然后一行 `200` 头和原样转发的上游 body），旁边是旧的 `/bitllm/infer/1.0.0`；服务端不认识 `1.1.0` 时客户端回退到它。节点通过模型名派生出的 key 在 DHT 里找服务方，用 `/bitllm/hello/1.0.0` 向服务方要最新的 announce，并把 gossip（`bitllm/announce/1`）作为空闲名额的快速路径。

## 参与贡献

Bug、修复和小功能都欢迎。[CONTRIBUTING.md](CONTRIBUTING.md) 说明 issue 和 PR 怎么处理；改代码前先读 [AGENTS.md](AGENTS.md)。改了网络、访问或者网关之后，跑一遍 external test workflow（`.github/workflows/external-test.yml`），它会加入线上网络并发送真实请求。

其他文档：[审计](docs/audit.md)、[托管 GPU](docs/host-a-gpu.md)、[seed 交接](docs/seed-handoff.md)、[网络地图](docs/network-map.md)、[兼容性](docs/compat.md)、[English README](README.md)，以及 [llms.txt](https://github.com/bitllm-org/landing-page/blob/main/public/llms.txt)（给 AI agent 的同一套说明）。品牌资源在 [bitllm-org/branding](https://github.com/bitllm-org/branding)。

## 许可证

MIT，见 [LICENSE](LICENSE)。
控制台内置 JetBrains Mono 和 Arimo 字体，采用 SIL Open Font License 1.1 许可；版权声明和许可证全文见
[internal/assets/fonts.css](internal/assets/fonts.css)。

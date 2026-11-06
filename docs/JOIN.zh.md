# 加入 BitLLM

> 用 AI agent 帮你装？把 https://bitllm.org/llms.txt 发给它，里面有一步一检查的安装和配置步骤。
> 这是一份快速上手；完整的说明（网络结构、seed、审计、构建）见 [README.zh.md](../README.zh.md)。

BitLLM 是一个本地大模型的点对点网络：把你自己跑的模型分享出来，也能用到别人分享的模型。没有中心服务器，不用做端口转发，不用注册账号。

## 1. 下载

复制对应系统的命令，安装脚本会自动选对版本、核对校验和，装到 `~/.local/bin`，不需要 sudo：

```bash
# Mac（Apple 芯片）、Linux
curl -fsSL https://bitllm.org/install.sh | sh
~/.local/bin/bitllm
```

```powershell
# Windows（PowerShell）
irm https://bitllm.org/install.ps1 | iex
& "$env:LOCALAPPDATA\bitllm\bitllm.exe"
```

然后浏览器打开 http://127.0.0.1:7778 ，选一个模型，复制给你的 agent 用的配置，点"发送测试消息"确认能用。想手动下载的话，到 Releases 页面下载对应系统的文件（Mac 上请用 curl 下载，避免被系统隔离）。

下面的命令都写 `bitllm`。如果 `~/.local/bin` 不在 PATH 里，就用完整路径 `~/.local/bin/bitllm`；Windows 上用 `& "$env:LOCALAPPDATA\bitllm\bitllm.exe"`。

## 2. 分享你的 DeepSeek V4.1 Flash 1017

假设你用 vLLM 或 SGLang 在本机 8000 端口跑模型，先查一下它对外叫什么名字：

```bash
curl -s localhost:8000/v1/models     # 看 "id" 字段，比如 "deepseek-v4.1-flash"
```

大多数情况下不需要映射，直接指向服务就行：

```bash
bitllm --upstream http://localhost:8000/v1 --concurrency 2
```

它会把服务列出的每个模型按原 id 分享，运行中也会重新读取列表，所以之后新加的模型不用重启。Hugging Face 的 id 保留斜杠和大小写（如 `Qwen/Qwen3.8-Flash-Next`）；同一个仓库在多台机器上分享时，客户端会把它们当成同一个模型，一台下线就换另一台。

只有本地服务叫的名字和网络名字不一样时才需要映射：

```bash
bitllm --upstream http://localhost:8000/v1 \
  --model deepseek-v4.1-flash=deepseek-v4.1-flash-1017 --concurrency 2
```

- 等号左边是你本地的名字，右边是网络名字。官方权重用 `deepseek-v4.1-flash-1017`，去审查版用 `deepseek-v4.1-flash-1017-heretic`；Qwen3.8 Flash-Next 的 NVIDIA FP4-Local 量化 + alesha 去审查微调版用 `qwen3.8-flash-next-fp4-alesha`，local-inference-lab 的 NVFP4-MXFP8-CSF-QAD 量化版用 `qwen3.8-flash-next-nvfp4-lil`。
- 命名规则：公开模型用 Hugging Face 原 id；微调或量化用实际权重的仓库 id，没有就在后面接 `-量化-来源`。私有或全新的模型随便取一个稳定的名字，节点会自动广播，客户端在 `/v1/models` 和 dashboard 里就能选。同一个名字必须对应同一套权重。
- `--concurrency` 是同时接几个请求，按你机器的余量填。多出来的请求留在这个节点的队列里等，先按访问等级、再按到达顺序轮；等久了等级每 20 秒升一级，最多升到最高的一级，之后按到达顺序，所以等得最久的排在最前面。队列满了直接拒绝，不让客户端反复重试。
- 来自其他节点的请求会被收窄：节点只转发已知字段，图片只收 `data:`，每个 `/completions` 请求只有一个 prompt。客户端发了不常见的字段时，可以用 `--peer-field NAME` 放行（可重复）。

看到 `sharing map[...]` 和 `gateway http://127.0.0.1:7778/v1` 就说明接上了。

## 3. 在 agent 里用网络里的模型

浏览器打开 `http://127.0.0.1:7778`，页面上有 Claude Code、OpenAI 兼容工具、pi 的现成配置，点复制就行，还有一个"测试连接"按钮。

本机接口同时支持 OpenAI 格式（`/v1/chat/completions`）和 Anthropic 格式（`/v1/messages`），所以 Claude Code 可以直接接：

```bash
M=deepseek-v4.1-flash-1017
ANTHROPIC_BASE_URL=http://127.0.0.1:7778 ANTHROPIC_API_KEY=bitllm ANTHROPIC_MODEL=$M \
ANTHROPIC_DEFAULT_OPUS_MODEL=$M ANTHROPIC_DEFAULT_SONNET_MODEL=$M ANTHROPIC_DEFAULT_HAIKU_MODEL=$M \
CLAUDE_CODE_SUBAGENT_MODEL=$M claude
```

Cline、Continue、OpenCode、Aider 等 OpenAI 兼容工具：Base URL 填 `http://127.0.0.1:7778/v1`，API key 随便填。命令行试一下：

```bash
curl localhost:7778/v1/models        # 网络里现在有哪些模型、各有几个节点
curl localhost:7778/v1/chat/completions \
  -d '{"model":"deepseek-v4.1-flash-1017","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

只想用不想分享，直接运行 `bitllm` 不带参数即可。你分享的模型留在你的机器上，由你自己的服务回答；你的服务挂了时，网关返回 503。加 `--peer-fallback` 才会改用其他节点，那些节点能读到 prompt。

## 4. 访问规则（邀请制内测，PT 站式规则）

1. 有邀请，任何空闲名额都能用。没被邀请的节点，只在服务方空闲时才会被服务，除非服务方选择对所有人开放。
2. 默认没有每日额度限制，服务方可以自己设置。
3. 分享算力就是贡献者：永远不受额度限制；节点服务过任务后，邀请名额从 3 个变成 10 个。

会员 = 根账户，或者被会员邀请的账户。你的节点只要服务过一次任务，就永久拥有 10 个邀请名额；即使服务方设置了每日额度，最近 7 天内服务过任务的贡献者也不受限制。根账户可以撤销一个账户，被撤销账户邀请进来的人也会一起失去资格（之后可以由别的会员重新邀请）。邀请由以太坊账户签名后全网广播，没有中心化的会员服务器。

怎么加入：把你的以太坊地址发给一位会员，请他邀请你，然后在控制台把你的节点关联到这个地址（见下面"积分"一节）。

邀请别人：用浏览器钱包的话，在控制台的 Invite 框里填对方地址；用助记词账户的话：

```bash
bitllm invite 0x对方地址
```

**只想分享给自己指定的几个人？** 启动时用 `--allow` 列出他们的节点 ID（每个节点启动时会打印自己的 ID，`curl -s 127.0.0.1:7778/status` 里的 `id` 也是）：

```bash
bitllm --upstream http://localhost:8000/v1 --model ... \
  --allow 12D3KooW...朋友1 --allow 12D3KooW...朋友2
```

- 名单里的节点不管有没有被邀请，都按会员对待，任何空闲名额都能用；`--free-daily` 照样生效。
- 名单外的节点一律拒绝（包括会员），对方会马上收到 `403 not allowed ... Your node is 12D3KooW...`，里面带着他自己的节点 ID，发给你就能加进名单。对方的节点会自动跳过你、去找别的服务方；如果只有你在分享这个模型，就直接报这个错，不会让人干等。
- 名单不会广播出去，所以别人的模型列表里仍然看得到你的模型。
- 不加 `--allow` 一切照旧，按上面三条规则。名单只在启动时读取，改了要重启节点。

## 5. 积分

你每帮别人跑一次推理，就得到一张双方签名的收据，记积分（prompt token ÷ 4 + 生成 token）。收据是双方自己上报的，两把密钥可能属于同一个人，所以它不能证明活真的干过。收据会广播到全网，任何人都能验证签名，但节点只能用它听到的收据算积分，所以不同节点的总数可能不一样：

```bash
curl localhost:7778/credits
```

积分是记在节点上的。想把自己几台机器的积分合到一个账户下，就把每个节点关联到同一个以太坊地址。这只是签一个名：不发交易、不花 gas、不上链。

```bash
bitllm account new --name 你的名字     # 生成新账户（12 个助记词，务必抄下来），并关联这台机器
bitllm account link --name 你的名字    # 在另一台机器上，输入同一组助记词关联
bitllm account                        # 查看这台机器归谁
```

也可以在控制台页面点"Link wallet"，用浏览器钱包（MetaMask、Rabby 等）签名关联。助记词就是账户本身，丢了就找不回来，别人拿到就能冒充你。

积分以后有什么用还没定。定之前会先公布计分规则：自己开小号刷自己、或者冒充模型的收据不算。

## 需要知道的

- **帮你跑推理的节点能看到你的 prompt 明文**（传输是加密的，但对方机器要读内容才能推理）。不要发敏感信息。
- 和你直连的节点能看到你的 IP，帮你推理的节点能看到你的节点 ID。在意的话，通过 VPN 使用。
- 你分享模型时，你服务的客户端也能看到你的 IP。
- 客户端对每个服务节点用一把签收据的密钥，每 30 天更换一次。这个服务节点能把你的任务互相关联起来，其他节点没法跨服务节点关联到你。收据显示哪个节点干了活，不显示谁问的。
- 节点理论上可以用别的模型冒充。启动时加 `--audit` 可以抽查给你干活的节点：偶尔给它发一个探针请求，让另一个节点逐 token 给回答打分，连续不合格的节点 24 小时内不再用于这个模型（`curl -s 127.0.0.1:7778/audits` 看结果）。默认的复核方是我们（维护者）自己跑的节点，用 `--auditor 节点ID` 换掉，`--auditor none` 关掉。能查出换模型、贪心请求下换成 Q4 或更粗的量化；查不出能认出探针的节点、只在采样请求上换量化、复核方说谎或和被查方串通；`--weights` 只是服务方自己声明的权重哈希，节点证明不了。数字和局限见 [audit.md](audit.md)。
- **在中国大陆连不上或很慢？** 默认的 bootstrap 种子来自 `bootstrap.bitllm.org` 的 DNS 列表，直连经常超时或握手很慢。列表为空或解析不到时，节点会自己起一个网络，并在日志里打印 join code（`bitllm --bootstrap ...`）。`HTTPS_PROXY` 只对 WebSocket 种子有用，目前只有 `boot.bitllm.org` 是这种种子；DNS 列表里的种子，以及节点之间的 TCP 和 QUIC 连接，都不走代理。有 HTTP 代理的话，这样启动可以经代理连上 `boot`：

  ```bash
  HTTPS_PROXY=http://127.0.0.1:7890 NO_PROXY=127.0.0.1,localhost bitllm
  ```

  `NO_PROXY` 保证本机的连接不走代理。地址换成你自己的代理。
- **IPv6 会把 p2p 端口暴露在公网上。** 机器有公网 IPv6、路由器又放行入站 IPv6 时，互联网上任何人都能连到 4001 端口（只是 p2p 端口；7778 的本机接口只监听 127.0.0.1，外面连不到）。这本来是为了让别人能直连你，但如果你不想在这台机器上开放端口，启动时加 `--ipv6=false`，只在 IPv4 上监听。代价：只有 IPv6 的节点没法直连你，更多流量要走中继，可能慢一些；IPv4 上一切照旧。
- 身份和账本存在 `~/.bitllm/`，换机器想保留积分就把整个目录带走。

有问题在群里问，或者在 GitHub 提 issue。

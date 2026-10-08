# AD_EXPLORER — 设计说明（Design Spec）

日期：2026-09-30
状态：待你 review
风格定稿：Win32 Classic（原型见 `prototype/index.html`）

---

## 1. 目标与范围

一个自托管的 Web 远程桌面网关：浏览器访问，Go 服务转发，能连 RDP 与（后续）VNC。
参照 [adfnekc/grdp](https://github.com/adfnekc/grdp) 作为协议实现（我方的依赖，不是被修改的对象）。

### M1 目标（本次要实现）

1. 控制台口令登录，未登录时任何接口都不可用
2. 主机的地址 / 端口 / 账号 / 密码保存在**浏览器**，**加密存储**，可导入导出
3. 能连上一台真实 RDP 主机（xrdp 或 Windows），看到画面
4. 键盘、鼠标可用，**含中文输入法**（走 Unicode 键盘事件）
5. 剪贴板：远端→本地进列表由用户主动取用，本地→远端走粘贴——详见 §8.4
6. 网页全屏

### M1 非目标（明确不做）

VNC、文件传输、剪贴板图片、音频、多用户与权限、会话录制与回放、移动端适配、H.264 透传。

### 关于 grdp 的前提

**本 spec 以 grdp 的 A/B/C 三项需求已实现为前提编写，不做任何临时方案。**
具体接口见 `docs/grdp-requirements.md`：

- A（P0）始终可用的合成帧缓冲 + 批量脏矩形 → §4.3 直接用，不再自建缓冲
- B（P0）指针缓存 + 位图化光标 → §4.3 直接用，不在网关里重复实现光标合成
- C（P0）Unicode 键盘事件上层 API → §5.3 直接用，不做"剪贴板 + Ctrl+V"这类中间方案

这三项未落地之前，`server/internal/rdp` 的对应方法无法实现；M1 的其余部分
（auth / vault / session / stream / ws / 前端）不受影响，可以先动。

---

## 2. 已定决策

| 决策 | 选择 | 影响 |
| --- | --- | --- |
| 视觉风格 | Win32 Classic | 原型已定稿，直接迁移为 token |
| 访问控制 | 单操作员 + 单口令 | 无用户表、无角色、无 ACL |
| 控制台口令来源 | 环境变量或命令行参数（明文） | 服务端不存文件，容器/进程参数即秘密载体 |
| 主机与凭据存储 | **只在浏览器**，服务端不落盘 | 网关无状态、无数据库、无主密钥 |
| 凭据加密 | **主密码 + AES-256-GCM 加密保险库**，详见 §7.1 | 多一个"解锁"步骤；主密码遗失即数据不可恢复 |
| 敏感操作门槛 | 查看密码 / 导出 / 改主密码 / 清空 均需重新验证主密码 | 详见 §7.2 |
| 剪贴板 | 远端→本地**不自动写入**本机剪贴板，进列表由用户主动 COPY | 详见 §8.4 |
| 画面传输 | 服务端合成帧缓冲 → 脏矩形 → WebP/JPEG | 依赖 grdp 需求 A |
| 输入 | 中文走 Unicode 键盘事件，不做临时方案 | 依赖 grdp 需求 C |
| 前端栈 | Vite + React + TS + Tailwind + shadcn | shadcn 只做行为组件，视觉全量覆盖 |
| 仓库结构 | monorepo：`server/` + `web/` + `prototype/` | 前端构建产物 embed 进 Go 二进制 |
| 控制台会话有效期 | N 小时（可配置，默认 8h） | Cookie 过期时间 |
| 保险库解锁保持 | N 小时（可配置，默认 8h） | 见 §7.1 对"保持"的实现取舍 |

> **两个必须分清的秘密**（文档与 UI 都要明确，否则用户一定会搞混）：
> 1. **控制台访问口令**——保护网关本身，存在服务端（环境变量/参数）。
> 2. **保险库主密码**——保护浏览器里的凭据，**从不发送给服务端**。
> 两者是不同屏幕、不同失败后果，登录页与解锁页要分开表达。
>
> **一处仍然接受的代价**：控制台口令以明文存在于进程环境 / 命令行。
> `ps`、`/proc/<pid>/environ`、容器 inspect 都能看到。
> 缓解：提供 `-password-file` 并只读挂载 secret 文件；启动时若检测到命令行传参则打印警告。

---

## 3. 架构

```
┌──────────────────────────────────────────────────────────┐
│ 浏览器（React SPA）                                       │
│  ├─ 加密保险库：localStorage 只存密文，主密码从不出浏览器  │
│  ├─ 主机列表 / 会话页 / 剪贴板日志 / Win32 Classic UI      │
│  └─ WS 客户端：JSON 控制帧 + 二进制画面帧                 │
└───────────────┬──────────────────────────────────────────┘
                │  HTTPS  →  POST /api/login, /api/sessions
                │  WSS    →  /api/sessions/{id}/stream
                ▼
┌──────────────────────────────────────────────────────────┐
│ Go 网关（单二进制，无数据库，无落盘）                      │
│  ├─ auth      控制台口令校验 + HMAC 会话令牌               │
│  ├─ http      静态资源(embed.FS) + REST + WS 升级          │
│  ├─ session   会话注册表：生命周期 / 上限 / 空闲回收        │
│  ├─ stream    合成帧缓冲 → 脏矩形合并 → 限帧 → WebP 编码   │
│  ├─ input     browser 事件 → scancode / Unicode / 鼠标     │
│  └─ rdp       grdp client 封装（明文密码仅存内存）         │
└───────────────┬──────────────────────────────────────────┘
                │  TCP 3389
                ▼
          RDP 目标主机
```

**关键不变量**

- I1：远端密码的**密文**只在浏览器的 localStorage；**明文**只在两处——浏览器解锁期间的内存，
  和网关进程的内存（会话存活期间）。永不写日志、永不写服务端文件、永不出现在错误信息里。
- I2：保险库主密码**从不发送给服务端**，不落任何存储（派生密钥存 sessionStorage，见 §7.1）。
- I3：控制台口令只存在于进程启动参数/环境变量/secret 文件，永不写日志。
- I4：网关重启后所有会话失效；浏览器凭据不受影响（重新连接即可）。
- I5：任何 API 在未通过控制台口令校验时返回 401，包括 WS 升级请求。

---

## 4. 组件分解

每个组件给出：职责 / 接口 / 依赖。目标是可以单独理解和测试。

### 4.1 `server/internal/auth` — 控制台访问控制

- 职责：校验控制台口令；签发与验证无状态会话令牌；登录限速。
- 接口：
  - `New(secret []byte, password string, ttl time.Duration, maxAttempts int) *Auth`
  - `Login(password string) (token string, err error)` — 常量时间比较
  - `Verify(token string) bool` — HMAC-SHA256 签名 + 过期检查
  - `RateLimit(key string) (allowed bool, retryAfter time.Duration)`
- 依赖：`crypto/hmac`、`crypto/subtle`
- 不依赖：HTTP、会话管理。这样它可以被独立测试。
- 令牌格式：`base64url(payload) + "." + base64url(hmac)`，payload = `{"exp":<unix>}`。
  签名密钥：启动时随机生成 32 字节（默认）或由 `-session-secret` 指定（多实例部署）。
- 限速：按客户端 IP，内存滑动窗口，5 次失败后锁定 10 分钟（与原型 UI 文案一致）。

### 4.2 `server/internal/vault` — 会话凭据持有（内存）

- 职责：在会话生命周期内持有远端密码，并保证释放与脱敏。
- 接口：
  - `Put(sessionID string, creds Credentials)`
  - `Take(sessionID string) (Credentials, bool)` — 取出即删除
  - `Purge(sessionID string)`
  - `Credentials.String() string` — 永远返回 `Credentials{host:…,user:…,password:[redacted]}`
- 依赖：无。
- 存在理由：给 I1 一个**唯一的落点**，让"密码不该出现在哪里"变成可审查的一件事，
  并让"日志里没有密码"这条测试有一个明确的靶子。

### 4.3 `server/internal/rdp` — grdp 封装

- 职责：把 grdp 的事件式 API 收敛成一个面向网关的接口；不泄漏 grdp 类型到上层。
- 接口：
  - `Dial(ctx, Config) (*Session, error)` — Config 含 host/port/user/password/protocol/width/height
  - `Session.Frames() <-chan Frame` — 合成后的脏矩形（BGRA → 已编码由上层决定，见 4.4）
  - `Session.SetCursor` 事件、`Session.Resize(w,h)`、`Session.Close()`
  - `Session.KeyDown/KeyUp(scancode uint16, extended bool)`
  - `Session.MouseMove/MouseDown/MouseUp/MouseWheel`
  - `Session.SetClipboardText/OnClipboardText`
  - `Session.Stats() Stats` — fps / 字节 / 延迟估算
- 依赖：`github.com/adfnekc/grdp/client`、`github.com/adfnekc/grdp/protocol/pdu`。
- **阻塞点**：`Frames()` 直接建立在 grdp 需求 A 的 `Framebuffer` + `OnFrame` 之上
  （见 `docs/grdp-requirements.md`），**不在网关里重复实现一份缓冲**。
  光标同理，直接消费需求 B 的 `OnCursor`。
  在这两项落地前，本包里对应的两个方法无法实现，其余方法不受影响。

### 4.4 `server/internal/stream` — 帧整形与编码

- 职责：脏矩形合并、限帧、编码、背压处理。这是唯一知道"网络贵不贵"的地方。
- 接口：
  - `NewEncoder(quality int, fps int) *Encoder`
  - `Encoder.Feed(dirty image.Rectangle, fb *image.RGBA) (<-chan []byte, error)`
  - `Encoder.SetQuality(q int)`
- 策略（**已实现，实测数据取代了原先的推测**）：
  1. **限帧在会话层**（`DefaultFrameInterval` 50ms，即 20fps，`-max-fps` 可调）。帧循环持有最新一帧，
     到 tick 才编码，间隔内到达的区域全部并入这一帧。新的帧**取代旧的像素**（发布的图像始终是
     "那一刻的整个桌面"），但**不取代它的区域消息**——区域先并入累加器，否则客户端会一直
     显示旧内容而没人告诉它重画。实测：**40 次更新 → 3 次编码**。
  2. 累加器合并脏区：重叠必合；不相交只在零浪费时合；总数超过 8 个就合并最便宜的一对；
     总面积 ≥ 桌面 60% 直接发整帧。
  3. 上一帧还没发完 → 像素池没有空闲缓冲 → 源**把区域带到下一次更新**一起发（**不排队、不丢弃**）。
     grdp 的脏区是精确的，丢了就永远不会再画。
  4. 持续丢帧 → 降质（步长 8，下限 25）；连续成功 → 慢速回升（步长 4），避免在链路边缘抖动。
- 编码器：**标准库 JPEG**。
- **关于编码成本，我先前判断错了，这里更正**：Go 的 `image/jpeg` **本来就有 `*image.RGBA` 的快路径**
  （`image/jpeg/writer.go` 的 `rgbaToYCbCr` 直接读 `Pix`），不存在"每像素一次接口调用"。
  成本在编码器本身，实测约 **30 ns/像素**：

  | | |
  | --- | --- |
  | 1280×720 整帧 | 32 ms（115 MB/s） |
  | 400×250 区域 | 3.7 ms |
  | 累加器（64 区域） | 28 µs |
  | 帧头 + 拷贝 | 216 ns |

  所以整帧每帧都编是负担不起的，这才有了第 1 条的限帧与合并。再快只能换编码器，
  而 WebP 意味着 cgo 或更慢的纯 Go 实现——这个项目不做这个交换。
- 依赖：`image`、`image/jpeg`。
- 背压的设计意图：宁可降质、掉帧，也绝不让内存因为排队而增长。

### 4.5 `server/internal/ws` — WebSocket 协议编解码

- 职责：帧的序列化 / 反序列化。纯函数，无状态，最容易测。
- 接口：
  - `WriteFrame(w io.Writer, f Frame) error` / `ReadControl(r io.Reader) (Control, error)`
- 依赖：`encoding/binary`、`encoding/json`。
- 测试：table-driven round-trip，含畸形帧、超长 payload、截断帧。

### 4.6 `server/internal/session` — 会话注册表

- 职责：`POST /api/sessions` → id；持有 `rdp.Session` + `stream.Encoder` + WS 连接；
  空闲回收、并发上限、优雅关闭。
- 接口：`Create/Get/Close/List/Shutdown`
- 依赖：4.2 4.3 4.4。
- 上限：默认同时 4 个会话（可配置）。超限返回 429 + 明确文案。
- 空闲回收：默认 15 分钟无输入且无 WS 连接 → 关闭。

### 4.7 `web/` — 前端

- `lib/vault.ts` — 加密保险库：KDF、AEAD 加解密、解锁/自动锁定、gate、导入导出（详见 §7.1）
- `lib/vaultModel.ts` — 明文模型（主机列表、设置）与校验，与存储层分离，便于单测
- `lib/ws.ts` — WS 客户端：重连、控制帧编解码、`ArrayBuffer` → `ImageBitmap` → canvas 贴图
- `lib/scancode.ts` — `KeyboardEvent.code` → RDP scancode 表（含 0xE0 扩展标志）
- `lib/ime.ts` — 输入法组字桥：`compositionstart/update/end` + `input` 事件 → `input.text` 帧
- `canvas/` — 画面渲染器：双缓冲 canvas、按脏矩形 `drawImage`、自动缩放、光标叠加
- 页面：`Unlock` / `Hosts` / `Session`
- 视觉层：`styles/win32.css` 从 `prototype/src/styles/win32-classic.css` 迁移，
  `--raised` / `--sunken` 保留为 Tailwind theme token

---

## 5. WebSocket 协议

一个 WS 连接对应一个会话。文本帧走 JSON 控制，二进制帧走画面。

### 5.1 二进制帧（服务端 → 客户端）

```
offset  size  field
0       1     type        0x01=画面, 0x02=光标, 0x03=文件块(M3)
1       1     format      0x01=WebP, 0x02=JPEG, 0x03=PNG(光标)
2       2     flags       bit0=整帧(忽略 x/y/w/h), bit1=关键帧
4       4     x, y, w, h  各 uint16 LE（脏矩形）
12      4     seq         uint32 LE，用于丢帧检测
16      4     payloadLen  uint32 LE
20      ...   payload
```

帧头固定 20 字节。光标帧的 payload 是 PNG，w/h 为热点坐标。

### 5.2 控制帧（JSON）

客户端 → 服务端：

| type | 字段 | 说明 |
| --- | --- | --- |
| `input.key` | `code`, `down`, `modifiers` | `code` 用浏览器 `KeyboardEvent.code` |
| `input.mouse` | `x`, `y`, `button`, `down` | 坐标是**远端桌面像素**，前端负责换算 |
| `input.wheel` | `x`, `y`, `delta` | |
| `input.text` | `text` | 需要走 Unicode 键盘事件的字符（CJK 输入法） |
| `clipboard.set` | `text` | 本地 → 远端 |
| `clipboard.request` | — | 请求远端剪贴板内容 |
| `session.quality` | `level` 0–100 | |
| `session.resize` | `w`, `h` | M2 才真正生效（需 grdp Display Control） |
| `session.close` | — | |
| `ping` | `t` | |

服务端 → 客户端：

| type | 字段 | 说明 |
| --- | --- | --- |
| `session.ready` | `width`, `height`, `name` | 首帧前发出 |
| `session.stats` | `fps`, `kbps`, `quality`, `dropped` | 每秒一次，驱动原型的 HUD |
| `clipboard.text` | `text`, `ts` | |
| `cursor.pos` | `x`, `y` | 可选，用于本地光标同步 |
| `error` | `code`, `message` | 见 §6 |
| `session.closed` | `reason` | |
| `pong` | `t` | |

### 5.3 输入映射要点

- `KeyboardEvent.code`（物理键）而非 `key`，避免布局差异。
- **（已在 grdp 源码确认）扩展键已支持**：`Client.KeyDown/KeyUp` 的第一个参数在
  `0xE000..0xE0FF` 区间时，低字节为 scancode、`0xE0` 前缀成为扩展标志，
  `name` 参数被忽略（`client/client.go:175-188`）。所以右 Ctrl/Alt、方向键、小键盘等可直接用。
- **（已在 grdp 源码确认）鼠标按钮为 0 基**：0=左、1=中、2=右，且这是发送方的约定
  （`client/client.go:203-215`）。
- **中文输入走 Unicode 键盘事件**（grdp 需求 C）。`input.text` 到达网关后调
  `Session.UnicodeKeyDown/KeyUp`（内部按代理对拆分）。**不做"剪贴板 + Ctrl+V"这类中间方案**：
  那种做法会污染远端剪贴板、无法处理组字中的逐字修正，而且会在日后替换时把两端协议都改一遍。
- `input.key`（scancode）与 `input.text`（Unicode）是**两条并存的通道**，不是替代关系：
  修饰键、功能键、方向键走 scancode；只有字符输入走 Unicode。顺序上修饰键先于字符事件下发。
- 浏览器拦截的快捷键必须显式处理或明确不拦截：`Ctrl+Alt+Del`（浏览器无法发送，
  改用工具栏按钮发 `Ctrl+Alt+End`，与原型 CAD 按钮一致）、`Ctrl+W`、`F11`、
  `Alt+Tab` 与 `Win` 键（浏览器/系统层拦截，无法发送，UI 上给按钮）。

---

## 6. 错误处理

远端连接失败必须给出可区分的错误，而不是"连接失败"。错误码：

| code | 触发 | UI 表现 |
| --- | --- | --- |
| `auth_required` | 无/过期 Cookie（401） | 跳登录页 |
| `login_failed` | 口令错误 | 登录框内联错误 + 剩余尝试次数 |
| `login_locked` | 触发限速 | 倒计时文案 |
| `host_unreachable` | TCP 拨号失败 | Win32 消息框：地址/端口/超时 |
| `tls_error` | 证书/握手失败 | 消息框 + 建议改协议为 `rdp` |
| `nla_failed` | CredSSP 失败 | 消息框：提示账号格式（`DOMAIN\user`）或协议 |
| `rdp_auth_failed` | 凭据被拒 | 消息框 + **回列表页并高亮该主机** |
| `protocol_error` | grdp 返回的协议错误 | 消息框 + 「复制诊断信息」（已脱敏） |
| `session_limit` | 并发超限（429） | 消息框 + 关闭一个会话的入口 |
| `session_gone` | WS 指向已回收会话（404） | 提示 + 回列表页 |

约定：
- 服务端错误一律不含密码，`vault.Credentials.String()` 是唯一的序列化出口。
- 客户端把 `error` 事件按 `code` 映射到文案；未知 code 显示原始 message + code（便于排障）。
- WS 异常断开：指数退避重连（1s/2s/4s，上限 10s，最多 5 次），失败后退回列表页并提示会话已结束。

---

## 7. 安全

**不变量**（§3 的 I1–I4）加上：

- 登录接口常量时间比较口令；失败与成功响应时间无差别。
- Cookie：`HttpOnly`、`Secure`、`SameSite=Lax`、`Path=/`。
- 所有写操作要求 `Origin` 校验，防 CSRF。
- 安全响应头：`Content-Security-Policy`（禁内联脚本，shadcn/Tailwind 产物可行）、
  `X-Content-Type-Options`、`Referrer-Policy: no-referrer`。
- WS 升级时校验 Cookie + `Origin`。
- 会话建立后 WS 只接受该会话的输入帧，不接受任何"切到别的 host"的指令。

### 7.1 保险库（Vault）

主密码只在浏览器，**从不发送给服务端**。目标：localStorage 里只有密文。

**存储格式**（单个条目 `adx_vault_v1`）

```json
{
  "v": 1,
  "kdf": { "algo": "PBKDF2-SHA256", "iterations": 600000, "salt": "<base64 16B>" },
  "cipher": { "algo": "AES-256-GCM", "iv": "<base64 12B>" },
  "tag_bits": 128,
  "data": "<base64 ciphertext>"
}
```

- 明文载荷是整个模型：`{ hosts: [...], settings: {...} }`，**主机名也在密文里**
  （主机名和账号一样敏感，不能为了渲染列表而泄露）。
- `AAD` = 版本号字符串（`"adx-vault-v1"`），防止把不同版本的密文对调。
- 密钥派生：`PBKDF2-SHA256(主密码, salt, 600k)` → 32B。选 PBKDF2 是因为 WebCrypto 原生支持、
  零依赖；将来要升级到 Argon2id 就引入 `hash-wasm`，存储格式已有 `kdf.algo` 字段可以平滑迁移。
- 每次保存都**重新生成 12B IV**，绝不复用。

**解锁与自动锁定**

- 解锁 = 派生密钥 → 解密成功。GCM 的 tag 本身就是主密码正确性的验证，不需要额外的 verifier。
- 派生密钥存在 `sessionStorage`，所以**刷新不掉**、关标签页即锁定。这是对"保持 N 小时"的安全解读：
  只要你还在干活就不会被反复打断，但密钥不落盘。
- **自动锁定**：无操作 N 小时（默认 8h，可配）后丢弃密钥与明文模型，回到锁定页。
- 一个已知取舍：若把派生密钥写 localStorage 并带过期时间，能跨标签页/重启保留解锁态，
  但那等于在磁盘上放了一把能解密全部凭据的钥匙，**等价于未加密**。
  因此**不提供这个选项**；如果你确实要，我单独加一个带醒目警告的开关。

**恢复**

主密码遗失 = 保险库不可恢复，无后门、无重置邮件。这是加密的必然代价，不是实现缺陷。
因此：
- 首次设置主密码时要求二次输入 + 强度提示 + 一个必须勾选的"我知道遗失后无法恢复"。
- 提供「重置保险库」（确认后销毁全部数据），作为唯一的退路，且它自己也要过 gate。

### 7.2 敏感操作闸门（Vault Gate）

以下四个操作**每次都要重新弹窗验证主密码**，即使当前已解锁：

| 操作 | 为何要 gate |
| --- | --- |
| 查看某个主机的密码（"显示密码"） | 防的是一次 XSS 或别人路过你屏幕就把密码读走 |
| 导出 | 导出是明文/可导入的完整凭据集，是最高价值目标 |
| 修改主密码 | 必须先证明是本人；且改密前允许查看旧密码（按你的要求） |
| 清空 / 重置保险库 | 不可逆操作 |

实现：gate 弹窗输入主密码 → 派生密钥 → 解密保险库验证 → 成功后放行**本次操作**
（不缓存放行状态，下次还要验）。gate 通过后把派生的密钥暂时保留在该次操作的内存作用域里，
操作结束即丢弃。

**导出格式**（两者都提供，默认加密）

- 加密导出：与保险库同一套 KDF/AEAD，导出时可另设口令（默认沿用主密码），
  便于放进云端/邮件传输。
- 明文导出：需要 gate + 二次确认弹窗，文件头写明 `"contains_plaintext_credentials": true`。

### 7.3 已接受风险

- 控制台口令明文存于进程环境/命令行（§2）。
- 保险库解锁期间，明文凭据在页面内存中。任何在那时的 XSS 仍能读到内存里的模型。
  加密防的是"静置数据的泄露"（磁盘、备份、浏览器同步），**不防运行时的 XSS**。
  这条要说清楚，不能拿加密当安慰剂。XSS 的主要防线是 §7 的 CSP。

---

## 8. 数据流

### 8.1 登录

```
Unlock 页 → 输入主密码 → PBKDF2 派生密钥 → 解密保险库
  ├─ 失败（GCM tag 不匹配）→ 内联错误，不泄露任何信息
  └─ 成功 → 派生密钥存 sessionStorage，明文模型进内存 → 跳 /hosts

Console 登录（另一个秘密，另一块屏幕）
Login 页 → POST /api/login {password}
  ├─ 失败 → 401 {attemptsLeft} → 内联错误
  └─ 成功 → Set-Cookie(adx_session) → 跳 /hosts
此后所有请求带 Cookie；WS 升级同样校验。
```

两个门的关系：**先过控制台门，再过保险库门**。已登录但未解锁时，`/hosts` 显示锁定态
（主机列表在密文里，解不出来），只有「解锁」按钮，不显示任何主机信息。

### 8.2 连接一台主机

```
Session 页 → 从 vault 取该 host 的明文凭据
  → POST /api/sessions {host,port,user,password,protocol,w,h}
      网关：session.Create() → rdp.Dial()（明文进 vault，内存）
      → 200 {id, width, height}
  → 打开 WS /api/sessions/{id}/stream
      网关 → session.ready → 首帧整帧 → 后续脏矩形
  浏览器：ImageBitmap → canvas → 脏矩形 drawImage
```

- 凭据不写日志；`POST` body 只在读取时解析一次，之后不再引用。
- 会话关闭（正常 / 断线 / 回收）→ `vault.Purge(id)`。

### 8.3 输入

```
keydown/keyup      → scancode.ts → input.key  → 网关 → grdp.KeyDown/KeyUp（含 0xE0 扩展键）
输入法组字结果      → ime.ts      → input.text → 网关 → grdp.UnicodeKeyDown/KeyUp
mousemove          → 按 canvas 缩放换算为远端像素 → input.mouse → grdp.MouseMove
```

- 鼠标移动做 16ms 节流；按下/抬起不节流（保证点击不丢）。
- 键盘 `e.preventDefault()` 只对已映射的键生效，避免吃掉浏览器自身的可访问性快捷键。
- **输入法桥**：中文/日文/韩文不能靠 `keydown`。监听 `compositionstart/update/end`，
  但**不发送**中间态（组字中的拼音/假名属于本地 UI），只把 `compositionend` 的最终结果
  以及非组字状态下的可打印字符走 `input.text`。
  组字期间发送 scancode 会被远端当成乱码输入，所以组字中必须抑制 `input.key`。
- 修饰键按 scancode 先行下发，字符事件随后；否则远端会收到「无 Ctrl 的 Ctrl 组合」。

### 8.4 剪贴板日志（不自动污染本机剪贴板）

按你的要求，远端内容**不直接写进本机剪贴板**，而是进一个日志，由你主动取用。
这同时解决了三件事：多会话互相污染、XSS 自动外流、以及浏览器写入剪贴板需要用户手势的限制。

**日志条目**

```
{ id, sessionId, hostName, direction: "in"|"out", text, bytes, ts }
```

- 每个会话的 `clipboard.text` 到达时**追加**一条（`direction: "in"`）；连续重复内容去重。
- 容量上限：50 条 / 1 MiB，超出后按 FIFO 淘汰。
- **只存内存**，刷新即清空，并提供「清空」按钮。
  理由：剪贴板里经常是密码、token、连接串，把它们写进 localStorage 与我们对保险库的加密立场矛盾。

**UI（对应原型的剪贴板面板）**

- 列表展示：来源主机 + 方向 + 时间 + 内容摘要（长文本折叠，可展开）。
- 每条一个 **COPY** 按钮，在 click 处理器里调 `navigator.clipboard.writeText`
  （就在用户手势里，因此不需要额外权限申请）。
- 默认只显示当前会话的条目，顶部一个「全部会话」开关。
- 可选：每个会话一个「自动同步到本机剪贴板」开关，**默认关**。
  开了就只对该会话生效——想省事的人不必每次点 COPY，又不会让其他会话污染本机。

**本地 → 远端**

- 监听 `paste` 事件（用户手势内可直接读 `clipboardData`），拿到文本发 `clipboard.set`，
  同时记一条 `direction: "out"`。
- 提供一个手动输入框：有些场景（如没焦点在 canvas 上）直接粘贴更顺手。
- 非安全上下文（http 且非 localhost）下 Clipboard API 不可用 → 面板顶部显示明确横幅提示，
  且禁用 COPY，不静默失败。

### 8.5 断线与回收

- WS 断开 → 保留会话 30 秒等重连；超时 → `rdp.Close()` + `vault.Purge()`。
- 空闲 15 分钟无输入 → 同上。
- 网关 SIGTERM → 遍历会话发 `session.closed{reason:"server_shutdown"}` 后关闭。

---

## 9. 待定（写代码前会用到，但不阻塞开工）

1. ~~WebP 编码器选型~~ **已定：标准库 JPEG。**
   实测成本在编码器本身（约 30 ns/像素），不在像素转换；换 WebP 要么引入 cgo（破坏单静态二进制），
   要么用更慢的纯 Go 实现。真正的收益来自 §4.4 第 1 条的限帧与合并，那一项已经做完并实测
   （40 次更新 → 3 次编码）。`stream.ImageEncoder` 接口把编码器隔离在一处，将来要换成本仍然很低。
2. 远端桌面尺寸策略：M1 用固定尺寸 + 选项，与原型的 resolution 分段器一致。
   注意**分辨率与缩放是两件事**，UI 上要分开表达（原型里已有两个分段器）：
   - `resolution` = 向 RDP 服务器请求的桌面尺寸（1280×720 / 1920×1080），改它需要重连（M1）
     或 Display Control（M2）
   - `fit` / `scale` = 本地 canvas 的显示缩放，不改远端分辨率，纯前端
   原型里把 `fit` 放在 resolution 分段器内是不严谨的，迁移到 React 时归到缩放那一组。

---

## 10. grdp 侧的依赖

完整的接口需求、语义要求与验收方式已单独成文：

> **`docs/grdp-requirements.md`** —— 给 负责修改 grdp 的人

汇总：

| 编号 | 优先级 | 内容 | 阻塞 ad_explorer 的 |
| --- | --- | --- | --- |
| A | P0 | 始终可用的合成帧缓冲 + 批量脏矩形 | M1 画面传输（§4.3） |
| B | P0 | 指针缓存 + 位图化光标 | M1 光标显示（§4.3） |
| C | P0 | Unicode 键盘事件上层 API | M1 中文输入（§5.3 / §8.3） |
| D | P1 | cliprdr 非 Windows 文件传输 | M3 文件传输 |
| E | P2 | Display Control 改分辨率 | M2 运行期改分辨率 |
| F | P2 | VNC 编码 + 文档修正 | VNC 体验 |
| G | 建议 | 无头网关示例 | 降低接入成本 |

**A/B/C 落地前，M1 无法完整交付**（前提已写在 §1）。
但这三项都局限在 `server/internal/rdp` 一个包里，所以 M1 的其余部分可以先动：

| 可先做 | 依赖 grdp |
| --- | --- |
| `auth` / `session` / `ws` / `stream` / 前端全部 | 无 |
| `rdp` 的连接、生命周期、错误映射 | 无 |
| `rdp` 的 `Frames()` / 光标 | A / B |
| `rdp` 的 `input.text` | C |

### 我方仍需在 grdp 上确认的事实（V1–V5）

- V1 **已确认：支持**。`KeyDown/KeyUp` 的 sc 在 `0xE000..0xE0FF` 时低字节是 scancode、
  `0xE0` 前缀成为扩展标志；`name` 被忽略。（`client/client.go:175-188`）
- V2 **已确认：0 基**。鼠标按钮 0=左、1=中、2=右，为发送方约定。（`client/client.go:203-215`）
- V3 `Setting.Width/Height` 与 `OnSurfaceReset` 的关系；`EnableEGFX` 开启后是否还需要 bitmap 路径。
- V4 `OnClose` / `OnError` 的语义与是否可重连（`Login` 是阻塞的，重连大概需要新建 Client）。
- V5 剪贴板：`SetClipboardText` 的调用是否需要先收到服务器的格式协商；快速连续调用是否会互相覆盖。

---

## 11. 测试策略

**Go**
- `auth`：口令比较、令牌签发/验签/过期、限速窗口、篡改令牌被拒（table-driven）。
- `ws`：帧 round-trip、畸形帧、截断帧、超长 payload、不依赖网络。
- `stream`：脏矩形合并（含相邻矩形合并、>60% 整帧、溢出边界）、丢帧降质、恢复升质。
- `input`：scancode 表全量断言 + 扩展键标志；Unicode 输入的代理对拆分与修饰键顺序。
- `clip`：剪贴板日志的容量上限、去重、FIFO 淘汰、清空。
- **脱敏测试**：构造一次完整登录+连接流程，捕获全部日志输出，断言不含密码字符串与 base64 变体。
- 集成：复用 grdp 的 `scripts/dev-rdp.sh` 起本地 xrdp，连上去断言收到至少一个非空整帧。
  这是"对着真实服务器跑"的最小版本，与 grdp 自己的验证哲学一致。

**前端**
- Vitest：`vault`（KDF/加解密/解锁/自动锁定/gate/导入导出/坏数据不崩/密文被篡改要报错）、
  `vaultModel`、`scancode`、`ime`（组字中不泄露 scancode）、WS 客户端状态机（重连、丢帧）。
- Playwright：解锁 → 主机列表 → 假网关（Go 的 httptest 起一个假 WS）→ 看到画面 → 全屏。
  假网关只发预先编码好的几帧，不需要真 RDP，因此可以在 CI 里跑。
- 视觉回归：截图对比 `prototype/index.html`，确保迁移到 React 后 Win32 Classic 没有走形。
- **加密回归**：断言 localStorage 中不出现任何主机名/账号/密码明文（对整个 storage 做字符串搜）。

---

## 12. 里程碑与验收标准

### M1（本次）

**访问控制**
- [ ] `ad_explorer -password-file=… -listen=:8443` 启动，日志不打印口令；命令行传参时打印风险警告
- [ ] 未登录访问 `/api/*` 与 WS 升级 → 401
- [ ] 登录成功 → Cookie 有效期 N 小时（默认 8h，`-session-ttl` 可配）
- [ ] 5 次登录失败锁定 10 分钟
- [ ] 并发上限 4，超限返回 429 且有明确文案

**保险库**
- [ ] 首次进入引导设置主密码（二次输入 + 强度提示 + 不可恢复确认）
- [ ] 刷新不掉解锁态；关标签页后需重新输入主密码
- [ ] 无操作 N 小时（默认 8h）自动锁定
- [ ] 主机增删改查，全部在密文里；localStorage 中搜不到任何主机名/账号/密码明文
- [ ] 密文被篡改 → 报错而不是静默解出垃圾数据
- [ ] 查看密码 / 导出 / 改主密码 / 清空 四项都需要重新验证主密码
- [ ] 导出两种格式（默认加密、明文需二次确认）；导入后数据一致
- [ ] 改主密码后旧密码失效、新密码可解，数据不丢
- [ ] 「重置保险库」可用且不可逆

**会话**
- [ ] 连上一台真实 RDP 主机，画面持续更新，键鼠可用（含 0xE0 扩展键）
- [ ] 输入法输入中文，远端正确出现（依赖 grdp 需求 C）
- [ ] 光标在多次形状切换后仍正常显示（依赖 grdp 需求 B）
- [ ] 全屏可用；分辨率可选 1280×720 / 1920×1080，本端 fit / 100% / 125% / 150% 缩放
- [ ] 断开后会话释放，内存里的密码被清除

**剪贴板**
- [ ] 远端复制文本后，本机剪贴板**未被改动**，面板里多出一条日志
- [ ] 点 COPY 才写进本机剪贴板
- [ ] 多会话并行时日志按来源正确标记；「全部会话」开关可用
- [ ] 本地粘贴能送到远端，并记一条 `out`
- [ ] 非安全上下文下显示横幅提示且禁用 COPY

**其它**
- [ ] 日志脱敏测试通过
- [ ] Win32 Classic 视觉与原型一致（视觉回归通过）

### M2 效率与体验
服务端脏区合并 + 自适应质量调优；断线重连；运行期改分辨率（依赖 grdp 需求 E）；H.264 透传。

### M3 文件传输 —— 已实现（grdp v0.8.0 的 `FileProvider` 到位后）

两个方向都通了，而且网关两边都不落盘：

- **本地 → 远端**：浏览器把文件登记给网关，网关装一个 clipboard provider 宣告它们。
  远端粘贴时按范围要字节，网关再**按需**向浏览器要那一段——4 GiB 的文件在任何一端都不必整个进内存。
- **远端 → 本地**：远端剪贴板的清单出现在面板里，下载把文件分块流回浏览器再拼装。

分块是二进制帧，**自带偏移量**，所以一块该放哪里由它自己说明，而不是由到达顺序决定。

验证方式：`-demo` 模式下 `demoFiles` 会把分享的文件当成远端文件宣告出来，
点下载就走完「offer → 清单 → 按范围取字节 → 分块下载 → 拼装」整个往返。
CDP 驱动的端到端检查对 200000 字节做了校验和比对，`bytesMatch: true`。

### M4 运维
审计日志、会话录制、HTTPS 与证书管理、多用户与 ACL、单二进制发布。

---

## 13. 未决问题（不阻塞 M1，但要在 M2 前定）

- 网关如何访问目标网段（同机 / 跳板 / VPN）？影响部署文档，不影响代码。
- 是否需要"只读旁观"链接？
- 是否需要把主机列表做成团队共享（会打破"服务端无状态"的前提）？

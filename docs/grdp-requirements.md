# grdp 需求单 — ad_explorer 网关所需的上层能力

面向：负责修改 [adfnekc/grdp](https://github.com/adfnekc/grdp) 的人
提出方：ad_explorer（Web 远程桌面网关）
基线：已 clone 并核对源码，`go build ./...`、`go vet ./...`、`go test ./...` 全部通过

---

## 核对结果（2026-10，v0.8.0 → v0.8.1）

已逐项对照源码，不是照 README 判断。**v0.8.1 之后全部关闭。**

| 编号 | 状态 | 证据 |
| --- | --- | --- |
| A 帧缓冲 + 脏矩形 | ✅ 完成 | `Client.Framebuffer()` 返回 `*Framebuffer`（`Bounds/Size/Stride/Pix`），`OnFrame(func([]image.Rectangle))` 已合并并裁剪；`Screen`/`OnOrdersFrame` 标为 deprecated |
| B 指针缓存 + 位图光标 | ✅ 完成 | `Client.OnCursor(func(*Cursor))`，`Cursor` 带 `Image *image.RGBA`、`Hotspot`、`System`、`SystemType`，`Hidden()` 处理 SYSPTR_NULL；`client/cursor.go` 注释说明 20 槽缓存已实现 |
| C Unicode 键盘 | ✅ 完成 | `client/unicode.go`：`UnicodeKeyDown/Up(r rune)`、`TypeText(s, hold)`，含代理对拆分与 `ErrNoUnicodeInput` |
| D cliprdr 非 Windows 文件传输 | ✅ 完成 | `FileProvider`（`Names/Info/Open`）、`SetFileProvider`、`OnRemoteFiles`、`ReadRemoteFile`、`Client.Files()`；`cliprdr_other.go` 改为经 provider 宣告文件格式 |
| E Display Control 改分辨率 | ✅ 完成 | `RequestResize(w,h)`、`OnResize(func(w,h int))`、`Setting.EnableDisplayControl` |
| F VNC 编码 | ✅ 基本完成 | Raw + CopyRect + Hextile；`RequestClipboardText` 在 RFB 上确实无法实现，代码与注释都如实说明（RFB 只能由服务端推送），这不是缺陷 |
| **H 明文打印口令** | ✅ v0.8.1 修复 | 那一行换成了只记 `unicode` 标志与响应长度的 Debug 日志，附了说明；**并且加了一个包内测试，再有日志调用被传入 password 字段就构建失败** |
| I 哨兵错误 | ✅ v0.8.1 修复 | 五个齐了：`ErrUnreachable`、`ErrTLSFailure`、`ErrCredSSP`、`ErrAuthenticationFailed`、`ErrSessionEndedByServer`；`client.go:249-283` 有中心分类器，覆盖所有失败路径 |
| **J 缩小桌面检测不到** | ❌ **未复现** | `client/rdp.go:677` 用 `wantW > screenW \|\| wantH > screenH`，缓冲区只增不减。试过两次真机都没能让服务器真的缩小，所以只是源码判断 |

下面各节的「现状」保留为提出时的记录，未改。仍然开放的只有 H 与 I。

---

## 背景：为什么需要这些

ad_explorer 是一个**无头服务端网关**：浏览器 ⇄ Go 网关 ⇄ RDP 主机。
grdp 目前是一个**桌面 GUI 客户端**库（`example/` 是 gxui 程序，`example/web.go` 是 socket.io 1.4 的历史遗留），
所以它的 API 面是围绕"有一个本机窗口可以画"设计的。网关没有窗口，且需要**多路并发会话**、
**按带宽整形**、**把光标和字符还原成浏览器能画/能发的形式**。

下面 8 项按优先级排列。A/B/C 是**开工硬阻塞**，H 是**凭据泄漏**（与 ad_explorer 无关，但建议优先），
D 是文件传输功能的前置，E/F/I 可延后。

每一项都给出：现状证据（文件:行号）→ 期望 API → 语义要求 → 验收方式。
验收沿用 grdp 自己的哲学：**对着真实服务器跑**，而不是写一份自己写、自己过的单测。
现成的 `scripts/dev-rdp.sh`（xrdp）与 `scripts/vnc-dev.sh`（TigerVNC）可以直接复用。

---

## A（P0）始终可用的合成帧缓冲 + 批量脏矩形

### 现状

- `Client.Screen()` 只在 `Setting.EnableOrders` 为真时非 nil
  （`client/client.go:257-265`，内部取 `RdpClient.screen`）。
- 默认的 bitmap 路径下，调用方只能从 `OnBitmap` 拿一堆矩形自己往缓冲里贴，
  而且 `Bitmap.Data` 的生命周期只有回调期间
  （`client/client.go:327` 注释明确说"f 可能看到同一份底层数据被下一次更新复用，需要保存就自己复制"）。
- `OnOrdersFrame` 每次只给**一个** `image.Rectangle`（`client/client.go:266`），
  一个更新批次里会回调多次，调用方要自己去重与合并。
- `OnSurfaceFrame` 给的是 `rdpgfx.Surface` 的指针，同样的生命周期问题（`client/client.go:275`）。

### 期望 API

```go
// Framebuffer 是解码结果的唯一落点。无论 EnableOrders 与否都存在。
type Framebuffer struct { /* ... */ }

func (c *Client) Framebuffer() *Framebuffer   // 永远非 nil（Login 之后）

func (f *Framebuffer) Bounds() image.Rectangle
func (f *Framebuffer) Size() (w, h int)
// Pix 是 BGRA、自上而下、stride = 4*Width。生命周期与 Client 相同，
// 内容在下一次 OnFrame 之前保持有效，可安全在回调外读取。
func (f *Framebuffer) Pix() []byte
func (f *Framebuffer) Stride() int

// OnFrame 在一个批次的更新全部贴完之后回调一次，dirty 已合并、已裁剪到 Bounds。
// slice 只在回调期间有效，需要保留请自行 copy。
func (c *Client) OnFrame(f func(dirty []image.Rectangle))
```

### 语义要求

1. **与 `EnableOrders` / `EnableEGFX` 解耦**：无论走 bitmap、orders 还是 surface bits，
   结果都落到同一个 `Framebuffer`。bitmap 路径下 `EnableOrders` 仍然为 false（不改变与服务器协商的能力），
   只是客户端把收到的矩形合成进缓冲。
2. **批次语义**：一个 PDU 批次内的多个矩形合并成一次 `OnFrame`，避免每矩形一次回调。
   网关侧按 30fps 做帧整形，逐矩形回调会让它做无意义的合并。
3. **合并规则**：相邻/重叠矩形合并；合并后若包围盒面积超过 `Bounds` 的 60%，直接返回整个 `Bounds`
   （省得调用方自己判断）。
4. **线程**：回调在解码线程上，网关会立刻 copy 出去，不需要额外同步承诺。
5. 保留 `Screen()` 与 `OnOrdersFrame` 的话请标记为 deprecated，不要同时存在两套落点。

### 验收

- xrdp + Windows 各一次：断言收到非空整帧；断言桌面内容与 `cmd/rdpcli -dump` 的 PNG 一致。
- 单测：合并规则（相邻、重叠、包围盒 >60% 整帧、边界溢出）。

---

## B（P0）指针：缓存 + 按位图还原

### 现状

- `protocol/pdu/pdu.go:49` 向服务器宣告 `CAPSTYPE_POINTER` 的
  `ColorPointerCacheSize: 20` —— 即**告诉服务器"我有 20 个指针槽"**。
- `PointerDataPDU.Unpack` 遇到 `TS_PTRMSGTYPE_CACHED` 时**只填 `CacheIndex`**
  （`protocol/pdu/data.go:1118`），`Data`/`Mask`/`Width`/`Height` 全为空。
- 全仓库没有任何指针缓存存储（`grep -rn pointerCache` 为空）。
- `OnPointer` 直接抛出原始 PDU（`client/client.go:248-255`），
  `cmd/rdpcli/main.go:353` 也只是打印字段。

### 后果

服务器完全有理由在第二个形状之后改用 `TS_PTRMSGTYPE_CACHED`，
此时客户端拿不到任何像素 —— **光标消失**。这是网关的硬阻塞项：浏览器必须画出一个光标。

### 期望 API

```go
type Cursor struct {
    // Image 是已应用 AND/XOR 掩码后的 RGBA，含 alpha，尺寸 = Width×Height。
    Image   *image.RGBA
    Width   int
    Height  int
    Hotspot image.Point      // 相对于 Image 左上角
}

// 形状或系统光标发生变化时回调。CACHED 已解析，不会给出空 Image。
func (c *Client) OnCursor(f func(cur *Cursor))

// 位置变化（沿用已有的 OnPointerPosition 也可以，这里只是给一个统一口径）
func (c *Client) OnCursorPos(f func(x, y int))
```

### 语义要求

1. **真的实现那 20 个槽**的缓存（或把宣告值改成 0 —— 但浏览器需要光标，所以请实现缓存）。
2. `TS_PTRMSGTYPE_COLOR` / `TS_PTRMSGTYPE_POINTER`：解出形状，写入缓存，构造 `Cursor`。
3. `TS_PTRMSGTYPE_CACHED`：从缓存取回完整形状再构造 `Cursor`；槽位为空则**不要**回一个空图像，
   应报错或跳过并记日志（静默给出空 Image 是最坏的结果）。
4. `TS_PTRMSGTYPE_SYSTEM`：这类是服务器内置光标（箭头/工字/沙漏等）。请仍然回调，
   但带一个可识别的标记，例如 `Cursor.System bool` + 一个枚举，让调用方可以退化成它自己的光标图。
   如果只给 `Width=0` 的 Cursor，调用方无法区分"系统光标"和"解码失败"。
5. `OnCursor` 只在形状变化时回调；纯位置变化不要触发（避免网关每帧重编码一张 PNG）。

### 验收

- Windows + xrdp：断言连续切换若干光标（箭头 → 文本工字 → 调整大小 → 忙碌），
  每一次都能拿到非空 `Image`，且热点落在图像范围内。
- 单测：缓存的存取与失效、掩码合成（AND 透明 + XOR 反色）。

---

## C（P0）Unicode 键盘事件的上层 API

### 现状

协议层**已经齐了**：

- `UnicodeKeyEvent` 结构体与序列化：`protocol/pdu/data.go:1044-1054`
- `INPUT_EVENT_UNICODE = 0x0005`：`protocol/pdu/caps.go:278`
- fastpath 事件码映射：`FASTPATH_INPUT_EVENT_UNICODE = 0x4`（`protocol/pdu/pdu.go:545`）
- 序列化分发已覆盖该类型：`protocol/pdu/pdu.go:560,591`

**但 `Client` 没有任何公开方法能发出它**：
`client/client.go:179-215` 只有 scancode 版 `KeyUp/KeyDown` 和鼠标。
`pdu.Client.SendInputEvents` 虽然存在（`protocol/pdu/pdu.go:608`），但 `client.RdpClient` 的
`pdu` 字段不可从包外访问，调用方只能自己重写整套 PDU 组装。

### 后果

浏览器里的输入法（中文/日文/韩文）打出的字符**无法送到远端**。
scancode 无法表达组字结果，这是 RDP 协议里 Unicode 键盘事件存在的唯一理由。

### 期望 API

```go
// 发送一次 Unicode 按键。codepoint 超出 U+FFFF 时内部按代理对拆成两次事件。
func (c *Client) UnicodeKeyDown(r rune)
func (c *Client) UnicodeKeyUp(r rune)

// 便利方法：按顺序按下并释放整串字符，带一个可选的字间隔（0 表示不限速）。
func (c *Client) TypeText(s string, hold time.Duration) error
```

### 语义要求

1. 协商了 fastpath 输入时走 `FASTPATH_INPUT_EVENT_UNICODE`，否则走慢路径 `INPUT_EVENT_UNICODE`
   （与现有 `KeyDown` 的双路径逻辑保持一致，可直接复用 `client/rdp.go` 里的那套分支）。
2. 字符 > U+FFFF：按 UTF-16 代理对拆成两个事件，两个都按下、再逆序释放。
3. `KeyboardFlags` 的处理要与 scancode 版一致（release 标志位）。
4. `TypeText` 里遇到无法表示的字符要返回错误，不要静默丢弃。

### 验收

- Windows + 中文输入法：断言 `TypeText("中文测试")` 后远端记事本/输入框里出现这四个字。
  这是唯一有意义的验收方式。
- 单测：代理对拆分、release 标志、fastpath/慢路径分支。

---

## D（P1）cliprdr 文件传输：非 Windows 构建可用

### 现状

- `plugin/cliprdr/cliprdr_other.go`（`!windows`）：
  - `GetFileNames() []string` 返回 `nil`
  - `GetFormatList()` 只宣告 `CF_UNICODETEXT` / `CF_TEXT`
  - `GetFileInfo()` 返回全 0
- `plugin/cliprdr/cliprdr.go:681-700` 的 `processFileContentsRequest`
  直接 `os.Open(core.UnicodeDecode(f.FileName))` —— **读的是本机文件系统的路径**。
- 也就是说：文件传输的实现**绑死在 Windows 的资源管理器**上，
  一个跑在 Linux 上的无头网关完全无法使用。

### 期望 API

把"文件从哪来、往哪去"抽象成接口，让应用自己决定（网关会接成一个内存/流式实现）：

```go
// 本地 → 远端：应用提供清单与字节流
type FileProvider interface {
    Names() []string
    Info(name string) (size int64, modTime time.Time, isDir bool)
    Open(name string) (io.ReadCloser, error)
}
func (c *CliprdrClient) SetFileProvider(p FileProvider)

// 远端 → 本地：应用按需拉取，不必整份驻留内存
type RemoteFile struct {
    Index uint32
    Name  string
    Size  int64
    IsDir bool
}
func (c *CliprdrClient) OnRemoteFiles(f func(files []RemoteFile))
// 读远端文件的 [off, off+n) 段。n<=0 表示读到结尾。
func (c *CliprdrClient) ReadRemoteFile(index uint32, off int64, n int) ([]byte, error)
// 主动放弃/清空当前远端文件集
func (c *CliprdrClient) ClearRemoteFiles()
```

### 语义要求

1. 设了 `FileProvider` 之后，`GetFormatList` 才宣告 `CFSTR_FILEDESCRIPTORW` + `CFSTR_FILECONTENTS`；
   没设就维持现状（只宣告文本），不要凭空宣告一个拿不出内容的格式。
2. `processFileContentsRequest` 的 `FILECONTENTS_RANGE` 分支改为经 `FileProvider.Open`
   定位读取，删掉对 `os.Open(路径)` 的依赖。
3. `FILECONTENTS_SIZE` 用 `Info()` 的 size。
4. 远端 → 本地：收到 `CFSTR_FILEDESCRIPTORW` 时解析出文件清单并回调 `OnRemoteFiles`，
   由应用决定要哪个、要哪一段；`CB_FILECONTENTS_REQUEST` 由应用触发。
5. 目录（`FILE_ATTRIBUTE_DIRECTORY`）：至少要在 `RemoteFile.IsDir` 里如实报告。
   递归传输可以由应用层展开，但**清单必须能表达目录**，否则用户拖一个文件夹过来会得到静默错误。
6. 单个文件大（GB 级）：`ReadRemoteFile` 必须支持分段，不允许为了给一个 `FILECONTENTS_RANGE`
   而把整个文件读进内存。

### 验收

- Windows 侧：远端资源管理器复制一个 2GB 文件 → 网关把清单与字节流完整取出，哈希一致。
- 反向：网关提供 3 个文件（含一个子目录）→ 远端粘贴出正确的文件与目录结构。
- 单测：`FILECONTENTS_RANGE` 的分段边界、`n<=0`、越界读取、目录清单序列化。

---

## E（P2）Display Control：运行期改分辨率

### 现状

全仓库没有 Display Control 虚拟通道（`grep -rn 'DisplayControl'` 为空）。
`Setting.Width/Height` 只在连接时生效（`client/client.go:421-425`）。

### 期望 API

```go
func (c *Client) RequestResize(w, h int) error
func (c *Client) OnResize(f func(w, h int))   // 服务器确认后的实际尺寸
```

通道：`Microsoft::Windows::RDS::DisplayControl`（DVC）。
需要支持 `CAPSTYPE_SIZE_AND_LAYOUT`（能力通告）与 `MonitorLayout`（MSGIDX 0x01/0x02/0x03）。
`OnResize` 之后 `Framebuffer` 的尺寸应随之变化，并触发一次整帧 `OnFrame`。

### 为什么需要

Web 前端全屏 / 自定义页面尺寸是**核心功能**。没有这个，用户改分辨率就得断开重连，
而重连意味着重新认证、丢失会话状态。

### 验收

- Windows：`RequestResize(1920,1080)` 后断言 `OnResize` 回调 1920×1080，
  且 `Framebuffer().Bounds()` 同步变化，桌面内容重新绘满。
- xrdp：可能不支持，断言返回明确错误而不是死等。

---

## F（P2）VNC：编码与文档

1. `sendSetEncoding`（`protocol/rfb/rfb.go:419-427`）只宣告 1 个编码：Raw（0）。
   而 `README.md` 写 "Raw and CopyRect encodings only"，
   `recvRectHeader`（`rfb.go:538-540`）对任何非 0 编码直接报错 ——
   **CopyRect 实际不可用，文档与实现不符**。请至少修正文档。
2. 建议补 **Hextile**（编码 5）：实现简单，带宽/CPU 收益大，是让 VNC 在广域网可用的最低门槛。
3. `RequestClipboardText` 在 VNC 路径上未实现（`client/rfb.go` 包注释里已如实记录）。

---

## G（建议）补一个无头网关的接入示例

`example/` 是 gxui 桌面程序，`example/web.go` 用 socket.io 1.4 且没有帧缓冲/脏矩形概念。
也就是说仓库里**没有任何"无头服务端"的参考实现**。

建议在 `cmd/` 下补一个小示例（例如 `cmd/rdpws`）：把 `Framebuffer` 的脏矩形编码成 WebP/JPEG，
经 WebSocket 发给浏览器，接收键鼠事件回灌。哪怕只有 150 行，也能把上面 A/B/C 三项的
正确用法固定下来，并且它是可运行的集成测试。

---

## H（P0）删除明文打印账号口令的那行日志

### 现状

`protocol/nla/ntlm.go:442`，在 CredSSP 握手的 NTLM 认证消息组装过程中：

```go
glog.Infof("user: %s, passwd:%s", n.user, n.password)
```

对上层而言这是默认路径：

1. `client.NewSetting()` 把 `LogLevel` 默认设为 `glog.INFO`（`client/client.go:473-479`）
2. `client.NewClient()` 调 `s.SetLogLevel()` 把该级别推入**进程全局** logger（`client/client.go:111`）
3. `glog` 默认写 stdout（`client/client.go` 的 `init()` 里 `log.New(os.Stdout, …)`）

所以按 README 的推荐方式接入（`NewSetting` → `NewClient` → `Login`，协议用 `nla`，也就是连 Windows 的推荐配置），
**每一次连接都会把远端账号口令以明文写进 stdout**。在容器里 stdout 就是容器日志，
再经日志采集器就成了长期留存、被多方可见的凭据。

### 请求

1. **删掉这行**，或者至少只打印用户名。这是安全修复，不是清理。
2. 建议同时把 `NewSetting()` 的默认 `LogLevel` 从 `INFO` 改为 `WARN` 或 `NONE`：
   一个库不应该在调用方没有明确要求的情况下，把一个包含凭据的打印通路默认打开。
3. 建议在 CI 加一条守卫：对现有日志语句做字符串检查，禁止把 password/secret 类变量作为打印参数。

### ad_explorer 侧的现状

我们把它压到了 `WARN`，并在 `NewClient` 之后**再调一次** `glog.SetLevel`，
以免将来 grdp 改动或其它调用方把级别改回去（`internal/rdp/session.go` 的 `effectiveLogLevel`）。
但这只是防御：库本身不应该把这个口子默认打开。

### 验收

- 对着一个启用 NLA 的真实主机连接，把 `LogLevel` 设为 `INFO`，捕获 stdout，
  断言其中**不包含**口令字符串（及其 base64 变体）。
- 顺带断言 `NewSetting().LogLevel` 不是 `INFO`。

---

## I（P1）错误分类：给出可 errors.Is 的哨兵错误

### 现状

`Login` 失败时返回的都是 `fmt.Errorf` 字符串，没有可以判定的类型：

| 来源 | 文本 |
| --- | --- |
| `client/client.go:108` | `[dial err] ...` |
| `client/client.go:161` | `client: connection closed before the session became ready` |
| `client/client.go:163` | `client: session not ready: ...` |
| `protocol/sec/*.go` | `sec: server certificate has no public key` 、`sec: encrypt premaster secret err: ...` … |
| `protocol/err/…` | `NODE_RDP_PROTOCOL_PDU_SEC_BAD_LICENSE_HEADER` 之类的码 |

调用方想区分「地址不通」「证书不对」「CredSSP 失败」「密码错误」，**只能去匹配错误文本**。
这正是 ad_explorer 里的 `internal/rdp.codeFor()` 在做的事，而它对 grdp 的任何措辞改动都极其脆弱，
并且会把归类错误直接暴露到用户看到的提示上。

### 期望 API

```go
var (
    ErrDial       = errors.New("grdp: cannot reach the host")
    ErrTLS        = errors.New("grdp: TLS handshake failed")
    ErrCredSSP    = errors.New("grdp: CredSSP/NLA authentication failed")
    ErrAuthFailed = errors.New("grdp: credentials rejected")
    ErrLicensing  = errors.New("grdp: licensing exchange failed")
    ErrClosed     = errors.New("grdp: connection closed")
)
```

底层各返回点用 `fmt.Errorf("%w: ...", ErrDial, err)` 之类的写法包装，保留现有文本作为上下文。

### 验收

- 对错误口令的 NLA 主机：`errors.Is(err, client.ErrAuthFailed)` 成立。
- 对不存在的地址：`errors.Is(err, client.ErrDial)` 成立。
- 自签证书：`errors.Is(err, client.ErrTLS)` 成立。
- 无需任何字符串匹配即可分类。

### ad_explorer 侧的现状

`internal/rdp.codeFor()` 做最佳努力匹配，并在 `docs/…design.md` §6 里把归类写成了对外契约。
哨兵错误到位后，`codeFor` 从一堆 `strings.Contains` 变成一个 `errors.Is` 的 type switch，
调用方接口不变。

---

## 汇总表

| 编号 | 优先级 | 内容 | 阻塞 ad_explorer 的 |
| --- | --- | --- | --- |
| A | P0 | 始终可用的合成帧缓冲 + 批量脏矩形 | M1 画面传输 |
| B | P0 | 指针缓存 + 位图化光标 | M1 光标显示 |
| C | P0 | Unicode 键盘事件上层 API | M1 中文输入 |
| D | P1 | cliprdr 非 Windows 文件传输 | M3 文件传输 |
| E | P2 | Display Control 改分辨率 | M2 运行期改分辨率 |
| F | P2 | VNC 编码 + 文档修正 | VNC 体验 |
| G | 建议 | 无头网关示例 | 降低接入成本 |
| H | **P0** | **删除 `protocol/nla/ntlm.go:442` 明文打印口令的日志** | **安全：网关按默认配置就会泄漏所有远端口令** |
| I | P1 | 可 `errors.Is` 的哨兵错误（尚缺 dial / TLS / CredSSP 三类） | 网关只能靠匹配错误文本分类，措辞一变就错 |

A/B/C/D/E 已在 v0.8.0 落地，ad_explorer 已经对着真实 Windows 10 主机跑通了画面与光标（见文末「真机验证记录」）；
H 与 I 在 v0.8.1 关闭。**除 J 之外没有未解决的需求，而 J 尚未复现。**

### ad_explorer 侧因 v0.8.1 做的改动

- `codeFor` 现在**先判断哨兵**，字符串匹配降级为「错误以别的路径到达、或某个包装丢掉了链」时的兜底，并已在注释里写明这一点。
- 加了一条不需要网络的测试：连 `127.0.0.1:1`（必然被拒）并要求 `errors.Is(err, client.ErrUnreachable)` 成立。
  它证明的是**分类来自哨兵而不是文本匹配**，同时守住错误链不被中途抹掉。
- 这条测试顺带抓到了我们自己的一个 bug：`redact` 用子串匹配，于是
  **一个单字符口令会把 `dial tcp` 洗成 `dial tc[redacted]`**，并且因为重写错误而丢掉整条链，
  把哨兵一起弄没。现在改成**按完整词元匹配**：真实泄漏的口令是一个字段或独立单词，
  不会藏在别的词里。

---

## J（P1）缩小的桌面检测不到

### 现状

`client/rdp.go:660-680`，`composite` 里：

```go
wantW, wantH := screenW, screenH
for _, b := range bs {
    if b.DestLeft+b.Width  > wantW { wantW = b.DestLeft + b.Width }
    if b.DestTop+b.Height  > wantH { wantH = b.DestTop + b.Height }
}
if wantW > screenW || wantH > screenH {
    c.resizeFramebuffer(wantW, wantH)
}
```

也就是说，客户端是**从服务器画到哪里**反推桌面尺寸的，而且判据是**严格大于**。

### 后果

1. **缩小永远检测不到。** 服务器把桌面改小之后，每一张位图都落在「已经太大」的帧内，
   `wantW`/`wantH` 永远不会超过当前尺寸，于是 `resizeFramebuffer` 不会被调用，
   `OnResize` 不触发，帧缓冲保持旧的大尺寸。调用方会按错误的尺寸去摆放桌面，
   多出来的区域是 `Screen.Resize` 初始化时的黑色。
2. **静止画面学不到新尺寸。** 尺寸变化只从「画到帧外」得知，没有确认应答。
   锁屏、登录提示这类不重绘的画面上，`RequestResize` 会成功返回，但 `OnResize` 永远不来。

第 2 点本身可以接受（尺寸只在有东西可画时才有意义），但第 1 点是缺陷：
一次 1280×720 → 800×600 的缩放在代码里没有任何路径能被发现。

README 的验收写的是「resizes the framebuffer to 1280x768」，那是一次**放大**（高度 720 → 768），
正好绕过了这个问题。

### 建议

`RequestResize` 的调用方知道它请求了什么，而 `disp` 也知道服务器通告的上下限。
方向是：不要只依赖「画到帧外」，让 Display Control 这条路径能主动把帧缓冲对齐到
生效的布局。服务器会就近取整，所以最终仍以服务器实际绘制为准——
但至少缩小要有路径被发现。

具体机制留给你，因为只有你手上有能复现的 Windows 主机。

### 验收

- 从 1280×720 请求 800×600：`OnResize` 报告生效尺寸，`Framebuffer().Bounds()` 随之变小。
- 请求一个服务器会取整的尺寸（720 → 768）：仍然报告服务器选的那个。

### 已尝试复现，但没能成功（2026-10 第二次真机跑）

`internal/rdp/real_server_test.go` 的 resize 探针会分别请求一次放大（1600×900）和一次缩小
（800×600），并记录**这段时间内所有脏区的并集**——如果并集落在请求的尺寸之内，
说明服务器确实应用了它，那么帧缓冲没跟着变就是客户端漏掉了。

第二次真机（同一台 Windows 10）的结果：

- 放大：**什么都没画**，所以既没有尺寸可信，也没有检测可谈。
- 缩小：画了 5 帧，并集是 `(0,0)-(1280,720)`，**超出了请求的 800×600**，
  也就是服务器压根没有应用这次缩放，仍然按 1280×720 重绘。

**结论：J 目前只是源码层面的判断，没有复现。** 上面那次真机的现象（`RequestResize` 成功返回、
`OnResize` 不触发）是「服务器没应用」造成的，不是「客户端漏掉」。
两种情况在这一层无法区分，需要一次服务器**确实缩小了**的连接才能定论。

那台主机当时停在锁屏的「另一用户已登录」提示上，桌面没有真正建立；
测试探针在有服务器应用了缩放时会**失败**，失败即命中 J。

---

## 真机验证记录（2026-10，Windows 10）

对着 `192.168.75.130:3389` 跑 `internal/rdp/real_server_test.go` 的结果，作为上面各项的实测依据。

| 项 | 结果 |
| --- | --- |
| 连接 / NLA | ✅ 一次成功（`nla: connected`） |
| 错误分类：服务器要求 NLA 时用 tls | ✅ `nla_failed`，原始文本为 `x224: RDP security negotiation failed (0x00000005): the server requires CredSSP (NLA)` |
| 错误分类：服务器拒绝 Standard RDP 时用 rdp | ✅ `host_unreachable`（表现为握手期间被 reset，这一层无法与「地址不通」区分） |
| **A 画面** | ✅ 12 秒内 **103 个批次、118 个矩形区域**；帧缓冲 1280×720，stride 5120；导出的 PNG 是完整可读的 Windows 10 锁屏（模糊壁纸、头像、"rdptest"、 "Another user is signed in…" 提示） |
| **B 光标** | ✅ 首个形状 **41×39，热点 (3,3)，1599 像素中 145 个不透明** —— 与 grdp README 自己记录的「41×39，145 到 352 个不透明像素」一致 |
| E 改分辨率 | ⚠️ 未能验证。画面停在锁屏的「另一用户已登录」提示上，`OnResize` 未触发；随后主机下线，无法重测。源码层面原因见需求 J |
| 口令是否进日志 | ✅ 整个流程中日志与错误文本都不含口令 |

一条操作提醒：那台主机上**已有其他用户登录**，连接会弹出「继续会断开对方」的提示。
测试因此只读不写——不发送任何键盘输入——否则一次误触就会把对方踢下线。

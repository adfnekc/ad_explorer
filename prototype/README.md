# AD_EXPLORER — 原型（Win32 Classic）

基于 [adfnekc/grdp](https://github.com/adfnekc/grdp) 的 Web 远程桌面网关前端。

**风格已定稿：Win32 Classic**（Windows Classic / Win2000 控件语言）。其余七套对比稿已删除。

## 看原型

直接双击 `prototype/index.html` 即可 —— 单文件、自包含、含本地字体，离线可用。

```bash
# 或起个服务
python3 -m http.server 8080 --directory prototype   # → http://localhost:8080
```

带演示交互：会话选中、标签切换、右侧面板切换、toast、锁屏遮罩（顶栏 LOCK 或 Esc）、全屏。

## 风格语言（实现要点）

| 元素 | 做法 |
| --- | --- |
| 底色 | ButtonFace `#d4d0c8` |
| 三维边框 | 2px 倒角，由四层 `inset` box-shadow 组成：外亮 `#fff` / 内亮 `#dfdfdf` / 内暗 `#808080` / 外暗 `#000`；凹陷用同一组反向 |
| 标题栏 | `linear-gradient(90deg,#0a246a,#3a6ea5 55%,#a6caf0)` + 白色 Tahoma 粗体，右上角 `− □ ✕` |
| 列表选中 | 深蓝 `#000080` 底 + 白字 + `1px 1px 0 #808080` 外阴影 |
| 进度条 | 凹陷轨道内 `repeating-linear-gradient(90deg,#000080 0 8px,transparent 8px 10px)` 分块 |
| 远端桌面 | `#008080` 青色 + 经典任务栏 Start 按钮，控制台窗口用深蓝标题渐变 |
| 圆角 | 全部 0 |

`--raised` / `--sunken` 两个 token 承载全部倒角，改一处即可全局调整。

## 目录结构

```
prototype/
├── index.html              # 生成物：单文件原型，直接打开就是它
├── fonts/*.woff2           # 本地字体（IBM Plex Mono × 3）
└── src/
    ├── template.html       # 页面外壳
    ├── body.html           # DOM（唯一真源）
    ├── app.js              # 交互
    ├── gen.py              # 生成器 → ../index.html
    ├── fetch-fonts.py      # 一次性下载字体 + 生成 fonts.css
    └── styles/
        ├── fonts.css       # 生成物
        ├── base.css        # 共享结构样式（全走 token）
        └── win32-classic.css  # token + 覆盖
```

改完跑：

```bash
python3 prototype/src/gen.py
```

## 布局注记

- 远程画面按 16:9 取「能塞进客户区凹槽的最大矩形」，凹槽比 16:9 更方时上下留白是几何必然；
  要减少留白就收窄左右两栏（当前 `--rail:264px --dock:308px`）。
- 控制条在 <1700px 时是**两层刻意布局**（上层视图控制，下层会话动作右对齐），不是随机折行。
- `.desktop` 是 flex 列、`.term` 占满任务栏以上区域，等同于远端应用最大化。

## grdp 侧待办（等你那边改完才能进 M1）

| 优先级 | 问题 | 影响 |
| --- | --- | --- |
| P0 | `protocol/pdu/pdu.go:49` 宣告了 `ColorPointerCacheSize: 20`，但全仓库没有指针缓存；`TS_PTRMSGTYPE_CACHED` 只填了 `CacheIndex` | 光标在第二次形状变化后丢失，无法交付 |
| P0 | `Client.Screen()` 仅在 `EnableOrders` 时非 nil | 网关拿不到统一合成帧缓冲，只能自己拼脏矩形 |
| P1 | `cliprdr_other.go` 的 `GetFileNames()` 返回 nil、`GetFormatList` 只宣告文本，`processFileContentsRequest` 走 `os.Open(本地路径)` | Linux 网关无法做文件传输 |
| P2 | 无 Display Control DVC | 改分辨率只能重连 |
| P2 | VNC 只宣告 Raw（`protocol/rfb/rfb.go:419`，README 却写 Raw+CopyRect） | VNC 广域网体验差；文档与实现不符 |

# v2aynn-web UI 重设计方案

> 状态：**已实施**（2026-09-20）
> 范围：仅 `internal/web/static/index.html` 一个文件
> 约束：保持单文件内联、无构建链、无外部 CDN 依赖

## 实施中的方案变更

原方案是**深色单主题**。实施前用户确认改为 **浅色为主、深色为辅**，因此：
- 浅色为默认主题，深色作为辅助主题保留
- 首次访问跟随系统 `prefers-color-scheme`，顶栏按钮可手动切换并记忆
- 浅色变量是**重新设计并实测**的，不是深色的反色

首版浅色上线后用户反馈「更丑了、按钮和布局太垃圾」，据此做了第二轮修复。
两轮之间暴露的问题记录在 **§三.5**，是这份文档里最值得看的部分。

---

## 一、审查结论

现有界面功能完整，问题全部集中在**一致性**上：没有一套变量，所有视觉决策都是"写这一行时当场决定的"。具体表现为 6 类：

| # | 维度 | 实测问题 |
|---|---|---|
| 1 | 视觉层级 | 顶栏 5 个按钮同尺寸同形状；「启动」「停止」互斥操作并列显示 |
| 2 | 色彩系统 | 零 CSS 变量，约 60 处颜色字面量（CSS 约 54 + JS 内联 6） |
| 3 | 排版 | 字号 7 档（9/10/11/12/13/14/15px），含 9px 不可读区间 |
| 4 | 间距 | 15 种间距值、4 套按钮 padding、5 种圆角 |
| 5 | 组件状态 | 零 `:focus`、零 `:disabled`、5 个弹窗手写重复、11 处 `alert()` |
| 6 | 响应式 | 侧栏固定 220px + 节点行 `min-width:360px` → 375px 屏横向溢出 |

### 关键缺陷：同一颜色承担多种含义

| 颜色 | 顶栏含义 | 内容区含义 |
|---|---|---|
| 绿 `#238636` | 启动 | 粘贴导入 |
| 蓝 `#1f6feb` | 真实测速 | 手动添加 |
| 红 `#b62324` | 停止 | 删除分组 |

用户学会的"绿色 = 启动"到内容区即失效。`#1f6feb` 单独一个色就承担了 5 种用途（激活行底色、编辑标签、真实测速、手动添加、粘贴）。

### 对比度实测（WCAG）

| 位置 | 前景 / 背景 | 比值 | 结论 |
|---|---|---|---|
| `.gcnt` 计数徽章（11px） | `#8b949e` / `#21262d` | **4.36:1** | **不合格**（AA 要求 4.5:1） |
| `.gns` 订阅地址（11px） | `#8b949e` / `#0d1117` | 5.30:1 | 勉强 |
| `.st` 顶栏状态（13px） | `#8b949e` / `#161b22` | 4.98:1 | 勉强 |

---

## 二、设计定位

| 项 | 结论 |
|---|---|
| 渲染设备 | 低资源 ARM64 电视盒子（TF 卡 / eMMC） |
| 观看设备 | 局域网内的**手机或电脑浏览器** |
| 使用场景 | 偶尔打开看状态、切节点、测个速 —— 不是长时间驻留的界面 |
| 技术约束 | 单文件内联、无 npm/node 构建链、`//go:embed` 单二进制 |

### 设计 DNA：「深空 · 静默」

保留深色（暗环境 + 用户已习惯），但把色相从 GitHub 的中性灰蓝收拢到**带冷调的深空灰**，并强制语义分离：

- **强调色 `--accent` 只表达"可交互 + 当前焦点"**，不再表达"重要"
- **语义色（success / warning / danger）只表达状态**，不再用作按钮底色
- 主操作唯一 —— 顶栏只有一个实心按钮

---

## 三、设计变量（完整 `:root`，可直接粘贴）

```css
:root{
  /* 表面层 */
  --bg-base:#0B0E14;
  --bg-surface:#12161F;
  --bg-raised:#1A1F2B;
  --bg-hover:#1E2430;
  --bg-active:#16233A;

  /* 描边 */
  --border-subtle:#1E2430;
  --border:#262D3D;
  --border-strong:#333C4F;

  /* 文字层（对比度对 --bg-base） */
  --text-primary:#E8ECF2;   /* 16.3:1 */
  --text-secondary:#9BA6B8; /*  7.9:1 */
  --text-muted:#7A8598;     /*  5.2:1 */

  /* 强调（6.8:1） */
  --accent:#4C9AFF;
  --accent-hover:#6BAEFF;
  --accent-soft:#4C9AFF1F;
  --accent-line:#4C9AFF59;

  /* 语义 */
  --success:#3FB950;        /* 7.6:1 */
  --warning:#D29922;        /* 7.7:1 */
  --danger:#F85149;         /* 5.8:1 */
  --danger-soft:#F851491A;
  --danger-line:#F8514959;

  /* 字号：7 档 → 5 档 */
  --fs-xs:11px;
  --fs-sm:12px;
  --fs-base:13px;
  --fs-md:15px;
  --fs-lg:18px;

  /* 间距：8pt 网格 */
  --sp-1:4px; --sp-2:8px;  --sp-3:12px;
  --sp-4:16px; --sp-5:24px; --sp-6:32px;

  /* 圆角：5 种 → 3 种 */
  --r-sm:6px; --r-md:8px; --r-lg:12px;

  /* 动效 */
  --ease:cubic-bezier(.16,1,.3,1);
  --dur:.15s;

  /* 字体 */
  --font-sans:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC",
              "Hiragino Sans GB","Microsoft YaHei",sans-serif;
  --font-mono:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,
              "Liberation Mono",monospace;
}
```

**全部 11 个颜色对 `--bg-base` 的对比度均已实测通过 WCAG AA。**

---

## 三.5 浅色主题变量 + 首版翻车的 6 个原因

### 浅色变量（对 `--bg-base:#F5F7FA` 实测）

| 变量 | 值 | 说明 |
|---|---|---|
| `--bg-page` | `#E9EDF3` | body 底色，比容器深，让容器"浮"起来 |
| `--bg-base` | `#F5F7FA` | 容器内底色、输入框 |
| `--bg-surface` | `#FFFFFF` | 顶栏、表头、弹窗 |
| `--btn-bg` | `#F1F4F8` | **按钮专用**，不能与顶栏同色 |
| `--btn-bg-hover` | `#E6EBF2` | |
| `--border-subtle` | `#DDE3EB` | 行分隔线 |
| `--border` | `#D3DAE4` | |
| `--border-strong` | `#B9C2D0` | 按钮 / 输入框边框 |
| `--bw` | `1px` | **边框宽度分主题**（深色为 `.5px`） |
| `--text-primary` | `#161A22` | 16.3:1 |
| `--text-secondary` | `#4A5464` | 7.1:1 |
| `--text-muted` | `#64707F` | 4.7:1 |
| `--accent` | `#1667D8` | 4.9:1 |
| `--accent-fg` | `#FFFFFF` | 白字压 accent 上 5.3:1 |
| `--success` / `--warning` / `--danger` | `#1A7F37` / `#9A6700` / `#CF222E` | 4.7 / 4.5 / 5.0:1 |
| `--app-ring` | `rgba(16,22,34,.07)` | 容器外阴影 |

### 首版翻车的根因

**深色主题靠明暗差建立结构，浅色主题靠边框和阴影建立结构。** 浅色的背景色彼此太接近
（`#F5F7FA` / `#FFFFFF` / `#FFFFFF`），把深色那套参数直接搬过来，结构会整体塌掉。

具体 6 处：

| # | 症状 | 原因 | 修法 |
|---|---|---|---|
| 1 | 居中容器"看不见"了，像被删掉 | 删掉了 `.app` 的 `box-shadow`；白底压白底又没阴影 | 加回 `box-shadow:0 0 28px var(--app-ring)`，并让 body 底色比容器深一档 |
| 2 | 按钮像隐形了 | 顶栏 `--bg-surface` 与按钮 `--bg-raised` 都是 `#FFFFFF` | 新增独立的 `--btn-bg` |
| 3 | 边框几乎看不见 | 照搬深色的 `0.5px`；浅色下色差小，0.5px 会被稀释 | 抽出 `--bw`，浅色 `1px` / 深色 `.5px` |
| 4 | 节点行糊成一片 | 删掉了 `.ni` 的分隔线 | 恢复 `border-bottom:var(--bw) solid var(--border-subtle)` |
| 5 | 延迟/操作被推到最右，中间空一大片 | 删掉了 `.c1` 的 `max-width`，名称列吃掉所有空间 | 恢复 `.c1{max-width:520px}` |
| 6 | 分组条目同样糊 | 同上，删掉了 `.gi` 的分隔线 | 恢复 |

### 另外两个必须分主题的变量

- **`--accent-fg`**：浅色 accent 上白字 5.3:1 合格；深色 accent `#4C9AFF` 上白字只有
  **2.85:1 不合格**，必须用深色字 `#0B0E14`（6.8:1）。两套主题共用一个文字色会直接违反 AA。
- **`--bw`**：见上表。

### 主题切换的实现要点

- 初始化脚本必须放在**样式表之前**，否则会先渲染一帧浅色再被深色覆盖，出现肉眼可见的闪白
- 优先级：`localStorage` 手动选择 > `prefers-color-scheme`
- 加了 `color-scheme:light/dark`，让原生 select 下拉、checkbox、滚动条自动跟随
- `<meta name="theme-color">` 随切换同步（移动端地址栏配色）
- 切换按钮图标显示的是**点击后会切到哪个主题**，不是当前主题

### 操作按钮显隐：失败方向必须安全

`.ni .acts`（测速 / 删除）默认**常显**，只在 `@media (hover:hover) and (pointer:fine)`
命中时才收起、靠 hover / 键盘聚焦唤出。

反着写（默认藏、用 `@media(hover:none)` 唤出）一旦媒体查询不被支持，
触屏用户就永远点不到删除按钮 —— 失败方向是"功能不可用"。
现在最坏情况只是"按钮一直显示"，属外观退化。

`hover` 与 `pointer` 同时判断，是因为触摸屏笔记本的 `(hover:hover)` 也会命中（有鼠标），
只判 hover 会让那类设备在触屏下点不到。

---

## 四、组件规范

### 4.1 按钮：1 套尺寸 × 4 语义

替换现有的 4 套尺寸（`5px 14px` / `3px 10px` / `3px 7px` / `6px 16px`）。

```css
.btn{
  display:inline-flex;align-items:center;justify-content:center;gap:6px;
  height:32px;padding:0 var(--sp-3);
  font:500 var(--fs-sm)/1 var(--font-sans);
  color:var(--text-secondary);
  background:var(--bg-raised);
  border:.5px solid var(--border-strong);
  border-radius:var(--r-md);
  cursor:pointer;white-space:nowrap;
  transition:background var(--dur) var(--ease),
             color var(--dur) var(--ease),
             border-color var(--dur) var(--ease);
}
.btn:hover{background:var(--bg-hover);color:var(--text-primary)}
.btn:active{transform:translateY(1px)}
.btn:disabled{opacity:.45;cursor:not-allowed;transform:none}

.btn-primary{background:var(--accent);border-color:var(--accent);color:#0B0E14}
.btn-primary:hover{background:var(--accent-hover);border-color:var(--accent-hover);color:#0B0E14}

.btn-soft{background:var(--accent-soft);border-color:var(--accent-line);color:var(--accent)}
.btn-soft:hover{background:#4C9AFF2E;color:var(--accent-hover)}

.btn-danger{background:var(--danger-soft);border-color:var(--danger-line);color:var(--danger)}
.btn-danger:hover{background:#F851492E;color:#FF6B63}

.btn-sm{height:26px;padding:0 10px;font-size:var(--fs-xs);border-radius:var(--r-sm)}
.btn-icon{width:32px;padding:0}
```

| 类 | 用途 | 旧写法 |
|---|---|---|
| `.btn-primary` | 主操作（顶栏启动） | — |
| `.btn`（默认） | 次要操作 | `.btn` |
| `.btn-soft` | 强调但非主（+ 节点） | `.btn.ac` |
| `.btn-danger` | 危险（停止、删除分组） | `.btn.sp` |

**注意**：`.btn-primary` 的文字色用 `#0B0E14`（深色）而非白色 —— 白字在 `#4C9AFF` 上只有 2.85:1，深色字有 6.80:1。

### 4.2 焦点与禁用

现状完全缺失。键盘 Tab 走查时看不到焦点在哪。

```css
:focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:var(--r-sm)}
```

`pingGroup()` 已设 `btn.disabled=true`，但 CSS 无对应样式，现在补上（见 `.btn:disabled`）。

### 4.3 滚动条

深色页面配系统亮色滚动条很割裂。

```css
*::-webkit-scrollbar{width:8px;height:8px}
*::-webkit-scrollbar-track{background:transparent}
*::-webkit-scrollbar-thumb{background:var(--border-strong);border-radius:4px}
*::-webkit-scrollbar-thumb:hover{background:#45506A}
```

### 4.4 弹窗：加 150ms 入场

```css
.modal{
  position:fixed;inset:0;z-index:100;
  display:flex;align-items:center;justify-content:center;
  background:rgba(0,0,0,.55);
  opacity:0;visibility:hidden;
  transition:opacity var(--dur) var(--ease),visibility var(--dur) var(--ease);
}
.modal.show{opacity:1;visibility:visible}
.modal .mp{
  background:var(--bg-raised);
  border:.5px solid var(--border-strong);
  border-radius:var(--r-lg);
  padding:var(--sp-4);
  width:460px;max-width:92vw;max-height:82vh;overflow-y:auto;
  transform:scale(.97);
  transition:transform var(--dur) var(--ease);
}
.modal.show .mp{transform:scale(1)}
```

标题从蓝色 `#58a6ff` 改为 `--text-primary`（蓝色应留给可交互元素）：

```css
.modal .mp h3{font-size:var(--fs-lg);font-weight:500;color:var(--text-primary);margin-bottom:var(--sp-3)}
```

### 4.5 Toast：替换 11 处 `alert()`

现状全部走 `alert()` —— 阻塞式、样式不可控。新增轻量 toast：

```css
#toasts{position:fixed;top:16px;right:16px;z-index:200;
        display:flex;flex-direction:column;gap:var(--sp-2);pointer-events:none}
.toast{
  background:var(--bg-raised);border:.5px solid var(--border-strong);
  border-left:3px solid var(--accent);border-radius:var(--r-md);
  padding:10px var(--sp-3);font-size:var(--fs-sm);color:var(--text-primary);
  max-width:320px;opacity:0;transform:translateX(120%);
  transition:transform .24s var(--ease),opacity .24s var(--ease);
}
.toast.in{opacity:1;transform:translateX(0)}
.toast.ok{border-left-color:var(--success)}
.toast.err{border-left-color:var(--danger)}
```

```js
function toast(msg,kind){
  const w=document.getElementById('toasts');
  const t=document.createElement('div');
  t.className='toast'+(kind?' '+kind:'');
  t.textContent=msg;
  w.appendChild(t);
  requestAnimationFrame(()=>t.classList.add('in'));
  setTimeout(()=>{t.classList.remove('in');setTimeout(()=>t.remove(),260)},2400);
}
```

**`confirm()` 保留原生**。删除分组 / 删除节点 / 导入覆盖配置都是不可逆操作，原生阻塞式确认框反而是合适的，改成自绘会降低警示强度。

---

## 五、六处结构改动

### ① 顶栏：主操作唯一

现状 5 个按钮同权重，且「启动」「停止」并列 —— 用户每次都要先判断"我现在该点哪个"。

改为：**一个按钮，文案与语义随状态切换**。

```html
<button class="btn btn-primary" id="powerBtn" onclick="togglePower()">启动</button>
```

```js
let running=false;
function togglePower(){ running?doStop():doStart() }
```

`loadStatus()` 里已拿到 `s.running`，顺手同步：

```js
async function loadStatus(){
  try{
    const s=await api('GET','/api/status');
    running=!!s.running;
    document.getElementById('dot').className='dot'+(running?' on':'');
    const btn=document.getElementById('powerBtn');
    btn.textContent=running?'停止':'启动';
    btn.className='btn '+(running?'btn-danger':'btn-primary');
    document.getElementById('st').textContent=
      (running?'运行中':'已停止')+(s.activeName?' · '+s.activeName:'');
    active=s.activeNode;
  }catch(e){document.getElementById('st').textContent='连接失败'}
}
```

「真实测速」与内容头的「测速」命名易混，顶栏改为 **「测速」**（走 `/api/speed`），内容头保留「测速」（走 `/api/ping/group`）。「设置 ⚙」去掉 emoji（`⚙` 在 system-ui 下基线与中文不对齐）。

### ② 色彩语义分离

见第四节按钮表。核心是：`success` / `danger` 不再作按钮底色，`accent` 不再表达"重要"。

协议标签从 4 种饱和色（`.tag.vmess/.vless/.trojan/.ss`）统一为中性底 —— 协议是次要信息，不该用 4 种饱和色抢注意力：

```css
.tag{
  display:inline-block;padding:0 5px;border-radius:var(--r-sm);
  font-size:var(--fs-xs);line-height:16px;
  background:var(--bg-raised);color:var(--text-secondary);
}
```

### ③ 节点行操作：常显 → hover 显现

现状每行两个按钮，50 行 = 100 个按钮同时喊叫。

```css
.ni .acts{opacity:0;transition:opacity var(--dur) var(--ease)}
.ni:hover .acts,.ni:focus-within .acts{opacity:1}
@media (hover:none){.ni .acts{opacity:1}}
```

`@media (hover:none)` 保证触屏设备上常显（否则手机上永远点不到）。`.acts` 内部改为文字按钮而非实心按钮：

```css
.ni .acts button{
  background:none;border:0;padding:2px 6px;cursor:pointer;
  font-size:var(--fs-xs);color:var(--text-secondary);border-radius:var(--r-sm);
}
.ni .acts button:hover{color:var(--text-primary);background:var(--bg-hover)}
.ni .acts button.del:hover{color:var(--danger)}
```

### ④ 数字排版

延迟与速度合并到同一列，改用等宽字体 + 等宽数字：

```css
.ni .c2{
  width:76px;text-align:right;flex-shrink:0;
  font-family:var(--font-mono);font-variant-numeric:tabular-nums;
}
.ni .c2 .ms{font-size:var(--fs-sm)}
.ni .c2 .sp{font-size:var(--fs-xs);color:var(--text-secondary)}
```

删除 `font-size:9px` 内联（在 `index.html:246`），速度提到 `--fs-xs`。

### ⑤ 焦点与禁用态

见 4.2。

### ⑥ 移动端

```css
@media (max-width:720px){
  .app{max-width:none;border-left:0;border-right:0}
  .main{flex-direction:column}
  .sidebar{width:auto;min-width:0;border-right:0;
           border-bottom:.5px solid var(--border-subtle)}
  .sidebar .sh{display:none}
  .sidebar .gl{display:flex;gap:var(--sp-2);overflow-x:auto;
               overflow-y:hidden;padding:var(--sp-2) var(--sp-3)}
  .sidebar .gi{flex-shrink:0;border-bottom:0;border-left:0;
               border:.5px solid transparent;border-radius:var(--r-md);
               background:var(--bg-raised);padding:6px 10px}
  .sidebar .gi.ac{border-color:var(--accent-line);background:var(--bg-active)}
  .sidebar .gi .gns{display:none}
  .nlh,.ni{min-width:0}
  .topbar{flex-wrap:wrap;row-gap:var(--sp-2)}
}
```

另把 `height:100vh` 改为 `height:100dvh`（`100vh` 在移动浏览器地址栏收放时会跳动），保留 `100vh` 作为降级：

```css
body,.app{height:100vh;height:100dvh}
```

---

## 六、改动清单

| 项 | 内容 |
|---|---|
| 改动文件 | `internal/web/static/index.html`（**唯一**） |
| CSS | 85 行 → 约 230 行（新增变量块 + 组件态 + 响应式） |
| HTML | 结构不变。仅：顶栏按钮加类名、`powerBtn` 加 id、节点行操作包一层 `.acts`、新增 `<div id="toasts">` |
| JS | `alert(` 11 处 → `toast(`；3 处内联颜色 → 类名；新增 `toast()` 6 行 + `togglePower()` 3 行；`loadStatus()` 内补 4 行 |
| DOM id | **全部保留**，无重命名 |
| 产物影响 | 预计 +3~4KB（gzip 后约 +1KB），对 6.4MB 二进制可忽略 |

### 明确不做（YAGNI）

- 不引入外部字体 / 图标库 —— 盒子常在内网，需离线可用，且不加体积
- 不引入构建链（Tailwind / PostCSS）—— 破坏单文件内联与单二进制部署
- 不做主题切换 —— 深色是唯一目标场景
- 不动任何 API 与后端逻辑

---

## 七、验收方式

1. **JS 语法**：导出 `<script>` 块 → `node --check`。⚠️ 本项目已知陷阱：内联 JS 改动 `go build` 查不出来，必须单独校验
2. **CSS 结构**：统计 `<style>` 块内 `{` 与 `}` 数量是否平衡（括号平衡不代表语义正确，但能挡住低级错误）
3. **DOM 契约**：正则提取全部 `getElementById('x')` 与 `id="x"` 求差集，确认无悬空引用；同时核对 `onclick` 调用的函数都有定义
4. **构建**：`GOOS=linux GOARCH=arm64 go build -ldflags="-s -w"`，用 `od -An -tx1 -j 18 -N 2` 确认 ELF 偏移 0x12 为 `b7 00`
5. **测试**：`go test -count=1 ./...` 全绿（当前 33 通过 / 1 跳过）
6. **产物核对**：比对 MD5。⚠️ **只看体积会误判** —— Go 段对齐会吸收小改动，实测出现过「体积完全相同但 MD5 不同」的情况
7. **视觉验证**：用本机 Chrome 无头模式截图（见下）

### 无头截图验证法

`agent-browser` 技能不支持 Windows，但可以直接调本机 Chrome：

```bash
CHROME="/c/Program Files/Google/Chrome/Application/chrome.exe"
"$CHROME" --headless=new --disable-gpu --no-sandbox --hide-scrollbars \
  --force-prefers-reduced-motion \
  --virtual-time-budget=5000 --window-size=1400,780 \
  --screenshot=C:/path/to/out.png http://127.0.0.1:8099/
```

三个关键参数，缺一不可：

- **`--force-prefers-reduced-motion`**：不加的话 CSS transition 在虚拟时间下**不会推进**。
  实测现象：按钮类名已从 `btn-primary` 切成 `btn-danger`、文字已变成「停止」，
  但背景仍显示旧的蓝色 —— 因为 150ms 的过渡停在 t=0。这会伪装成「样式没生效」的假 bug，
  排查了很久。加了它（配合产品里的 `prefers-reduced-motion` 支持）才是真实终态。
- **`--virtual-time-budget`**：等 JS 跑完（状态文字、分组列表、节点列表都靠 JS 渲染）。
- **`--window-size` 与视口不等高**：无头 Chrome 会给模拟的浏览器边框留 **95px**。
  `--window-size=1400,780` 的实际视口是 **685px 高**。不知道这点会把「视口外」误判成「容器没撑满」。

深色主题用 `--blink-settings=preferredColorScheme=0` 强制。

### 这套方法的盲区（必须承认）

- **触屏行为验证不了**：`--touch-events=enabled` 不改变媒体查询结果，
  无头 Chrome 永远报告 `hover:hover` / `pointer:fine`（已实测确认）。
  所以 `@media (hover:hover) and (pointer:fine)` 的实际效果只能由用户在真机上确认。
  这也是为什么该规则要按「失败方向安全」来写（见 §三.5）。
- 无法验证 hover 态、下拉菜单展开、弹窗动画等交互过程，只能验静态终态。
- 像素采样能定位「某个元素是不是这个颜色」，但判断不了「好不好看」。

**结论：无头截图能把「明显坏掉」的挡在交付前，但视觉验收仍然必须由用户完成。**

---

## 八、确认结果与实施记录

用户的确认（2026-09-20）：

| # | 事项 | 结论 |
|---|---|---|
| 1 | 顶栏「启动/停止」合并为单按钮 | ✅ 能 |
| 2 | 节点行「测速/删除」改为 hover 显现 | ✅ 可以 |
| 3 | 配色 | ⚠️ **改为「浅色为主、深色为辅」**（原方案是深色单主题） |
| 4 | 浅色主题 | ✅ 需要，且为主要主题 |

### 实施清单

- 双主题变量体系（浅色默认 + 深色辅助），顶栏图标切换，`localStorage` 持久化
- 顶栏：5 按钮 → 状态点 + 状态文字 + 单电源按钮（文案/语义随 `running` 切换）+ 更新全部 + 测速 + 主题 + 设置
- 内容头：「删除分组」改为 `⋯` 下拉菜单（含「编辑分组」「删除分组」），按分组是否有订阅 URL 决定 disabled
  → 顺带解决了「移动端分组条目 hover 编辑不可达」
- 分组条目：去掉 hover 编辑标签，只剩名字 + 计数
- 节点行：操作按钮按「失败方向安全」显隐；恢复行分隔线；恢复名称列 `max-width:520px`
- 延迟/速度合并同列，等宽字体 + `tabular-nums`（延迟正常为中性灰，仅超时转红）
- 11 处 `alert()` → toast；`confirm()` **保留原生**（不可逆操作，原生警示强度更高）
- 协议标签 4 种饱和色 → 统一中性底
- 响应式 `<720px`：侧栏折叠为横向 chip 条；`100vh` → `100dvh`；窄屏放宽列 `min-width`
- 无障碍：补齐 `:focus-visible`、`:disabled`，新增 `prefers-reduced-motion` 支持
- 滚动条、`color-scheme`、`theme-color` 跟随主题

### 验证结果

| 项 | 结果 |
|---|---|
| 内联 JS 语法 | 2 段 `node --check` 通过 |
| CSS 大括号 | 126 / 126 平衡 |
| DOM 契约 | 无悬空 id；`onclick` 函数全部有定义 |
| 遗留引用 | `delGrpBtn` / `activeGrp` / `doStart(` / `doStop(` 已清除 |
| `go vet` / `gofmt -l` | 干净 |
| `go test -count=1 ./...` | **33 通过 / 1 跳过**（与改动前基线一致） |
| ARM64 产物 | 6553762 字节，ELF `0x12 = b7 00`（AARCH64），MD5 `306541f18c2f279e2c437f5cc6480b5c` |
| 视觉 | 浅色/深色桌面 + 浅色移动端，无头截图逐项核对 |

### 体积对照实验

新二进制比旧的大 **正好 65536 字节（64KB）**，而 HTML 只增加 14594 字节，差值可疑。
用 `git show HEAD:...` 还原旧 HTML 重新编译，得到 **6488226 字节，与旧二进制分毫不差**。

结论：多出的约 50KB 是 Go 链接器按 **64KB 段边界对齐**的填充，不是内容。
这次对照顺带证明了构建可复现。

### 尚未验证（需要用户在真机确认）

- **触屏下的操作按钮常显** —— 无头 Chrome 无法模拟 `hover:none` / `pointer:coarse`（见 §七）
- hover 态、`⋯` 菜单展开、弹窗入场动画等交互过程
- 真机（电视盒子）上的实际观感

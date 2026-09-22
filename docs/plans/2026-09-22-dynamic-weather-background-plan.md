# 动态天气背景 + 视觉细节 实施计划

**目标**：把静态三色渐变背景升级为随天气变化的动态氛围（CSS 分层 + 单层 canvas 粒子），并提升卡片层次与数据可读性。

**架构**：不动布局与 DOM id。背景拆为 `.bg-sky` / `.bg-cloud` / `.bg-glow` / `.bg-flash` 四个固定层加一个 `canvas#fx`；配色由"7 组基准色 + 统一昼夜调制"生成；粒子引擎用单个 rAF 循环，按天气 kind 切换模式与密度。

**技术栈**：原生 HTML/CSS/JS（零依赖、单文件、由 `go:embed` 内嵌）。

**流程适配**：不拆 subagent（所有改动同一文件）；不写提交步骤（用户未要求提交）。

---

## Task 1：背景分层与配色体系

**文件**：`web/index.html`（CSS 区 + `THEMES`）

- 把现有 `.bg` 拆为 `.bg-sky`（保留原 3 层渐变）、`.bg-cloud`（多层 radial-gradient 软云）、`.bg-glow`（暖光斑）、`.bg-flash`（闪电覆盖层），全部 `position:fixed; z-index:-1; pointer-events:none`
- 新增 CSS 变量：`--accent`（Hero 辉光）、`--card-veil`（卡片暗遮罩）
- `THEMES` 改为每 kind 一组基准色 + 统一昼夜调制函数（夜间降明度、降饱和、加冷）

**验证**：页面正常渲染；控制台无错；`document.documentElement.style.getPropertyValue('--c1')` 随天气变化。

## Task 2：canvas 粒子引擎

**文件**：`web/index.html`（新增 `<canvas id="fx">` 与 JS 模块）

- 模式：`rain`（雨丝 + 风速倾角）、`snow`（雪花 + 横向摆动 + 大小差）、`fog`（柔光雾絮横移）、`none`
- 密度：小雨 60 / 中雨 140 / 大雨 220 / 雷暴 240；雪 80~160（按 code 73/75 分档）
- 倾角：`wind_speed_10m` 映射到 `-22°~+22°`
- 性能：粒子数按视口面积与 `devicePixelRatio` 计算并设硬顶；`resize` 防抖重建；`document.hidden` 暂停并在恢复时重置时间基准

**验证**：注入各 weather_code 观察模式与密度；浏览器 Performance 面板确认只用 transform/opacity 语义（canvas 绘制不触发布局）。

## Task 3：雷暴闪电 + reduced-motion 降级

**文件**：`web/index.html`

- 闪电：仅 `thunder`，随机 3~6s，二次闪（120ms 亮 → 80ms 暗 → 90ms 亮），只改 `.bg-flash` 的 opacity
- `prefers-reduced-motion: reduce`：不启动 canvas、关闭云/光斑/闪电动画（仅保留静态渐变，仍随天气换色）

**验证**：`setMedia`/CDP 模拟 reduced-motion → 无 canvas 元素绘制、无动画；雷暴态观察到周期性闪烁。

## Task 4：视觉细节 8 项

**文件**：`web/index.html`（CSS 为主）

1. Hero：92px / 字重 300 / `tabular-nums`；图标辉光；副标题三胶囊
2. 卡片 3 档层次 + 双层阴影 + 上亮下暗 1px 渐变描边
3. 指标：单位与数值分色
4. 逐小时折线改平滑曲线（Catmull-Rom），节点只留首尾与极值
5. 7 天温度条按冷暖做蓝→橙渐变
6. 4 级字号 + 8px 间距栅格
7. emoji 统一饱和度与基线
8. 卡片暗遮罩保证对比度 ≥ 4.5:1

**验证**：逐项肉眼核对 + 对比度计算；提醒条/错误卡片/骨架屏/toast 未被破坏。

## Task 5：验收实测

| 项 | 命令/方法 | 期望 |
|---|---|---|
| 控制台 | `npx @playwright/cli console` | 0 errors / 0 warnings |
| 请求域名 | Performance API | 仅本机 |
| 7 态切换 | 注入 weather_code | 背景与粒子正确 |
| 375px | resize + `scrollWidth` | == innerWidth |
| reduced-motion | 媒体模拟 | 无动画、仍变色 |
| 后台暂停 | 切标签页 | rAF 停止 |
| 回归 | `go test ./...`、`go test -race ./...` | 全绿 |

## Task 6：清理与交付

删除验证用临时文件（截图、`.playwright-cli`），确认仍是单个 `index.html`、无新增资源文件。

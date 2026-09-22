# weather-go

一个用 Go 标准库实现的本地天气预报服务：**单个可执行文件，零第三方依赖**，自带随天气变化的动态界面，适合放在本机或家庭服务器上自用。

## 功能

- **城市搜索**：中文直达区县。按「高德 → 和风 → Open-Meteo」三级回落，任一级未配置凭据时自动降级，不配置任何 Key 也能直接运行。
- **定位取名**：浏览器 GPS 定位后，经高德坐标转换与逆地理编码，显示真实城市名与区县，而不是一串经纬度。
- **天气预报**：实况、24 小时气温曲线、7 天预报（Open-Meteo）。
- **空气质量**：US AQI、PM2.5 / PM10（Open-Meteo）。
- **生活参考**：穿衣、紫外线、运动、洗车、空气防护等生活指数，以及雨伞 / 墨镜 / 防晒霜等出行携带清单（本地规则推算，仅供参考）。
- **动态背景**：天空配色、云层、太阳、月亮星星、雨雪雾粒子、雷暴闪电随当前天气与昼夜自动变化；系统开启"减少动效"时自动静止。
- **工程细节**：上游响应缓存（默认 10 分钟）+ 并发请求单飞合并；服务默认只监听回环地址；错误信息不泄漏上游 URL；API Key 只走环境变量。

## 快速开始

要求 Go 1.26+。

```bash
# 克隆并进入目录
git clone https://github.com/Abeautiful-crypto/weather.git
cd weather

# 构建
go build -o weather .
```

运行（PowerShell）：

```powershell
$env:AMAP_KEY = "你的高德Key"   # 可选，见下文配置说明
./weather.exe
```

运行（Linux / macOS）：

```bash
export AMAP_KEY=你的高德Key   # 可选
./weather
```

浏览器打开 <http://127.0.0.1:8080> 即可。

## 配置

### 环境变量

所有凭据均可缺省，缺省时对应功能自动降级：

| 变量 | 说明 |
|---|---|
| `AMAP_KEY` | 高德 Web 服务 Key。配置后城市搜索优先走高德（中文覆盖到区县），并启用定位取名。申请：[高德开放平台](https://lbs.amap.com/)，个人认证开发者每月免费额度充足 |
| `QWEATHER_HOST` | 和风天气专属 API Host，与 `QWEATHER_KEY` 成对配置才启用 |
| `QWEATHER_KEY` | 和风天气 API Key |

高德 Key 也可以用**本地文件**提供：在项目根目录创建 `amap.key`（一行纯文本，写入 Key 即可）。这在 IDE 运行、双击二进制等不方便设环境变量的场景更省事。读取优先级：环境变量 `AMAP_KEY` → `amap.key` 文件。该文件已加入 `.gitignore`，不会进入仓库。

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | HTTP 监听地址。默认仅本机可访问；需要局域网访问时显式指定 |
| `-cache-ttl` | `10m` | 上游响应缓存时长，`0` 表示不缓存 |
| `-upstream-timeout` | `8s` | 单个上游请求超时 |
| `-log-level` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `-web-dir` | 空 | 指定目录后前端从磁盘读取（改完刷新即生效，开发用）；留空使用内嵌资源 |

## API

服务本身只做代理与缓存，前端所有请求都走同源 `/api/*`：

| 接口 | 说明 |
|---|---|
| `GET /api/geocode?q=` | 城市搜索，三级回落；响应头 `X-Geocode-Source` 标注实际使用的数据源 |
| `GET /api/regeo?lat=&lon=` | 坐标 → 地名（依赖高德，未配置 `AMAP_KEY` 时返回 503） |
| `GET /api/weather?lat=&lon=&tz=` | 天气预报（Open-Meteo），`tz` 为 IANA 时区名 |
| `GET /api/air?lat=&lon=` | 空气质量（Open-Meteo） |
| `GET /api/health` | 健康检查 |

## 项目结构

```
weather-go/
├── main.go              # 入口：flag 解析、静态资源来源、优雅关停
├── internal/
│   ├── api/             # HTTP 处理器：/api/* 路由、地理编码回落链、静态文件
│   ├── amap/            # 高德客户端：地理编码 / 逆地理 / 坐标转换
│   ├── qweather/        # 和风客户端（可选启用）
│   ├── upstream/        # 上游 HTTP 客户端与统一错误
│   └── cache/           # 带单飞合并的 TTL 缓存
└── web/                 # 单文件前端（embed 进二进制）
```

## 开发

```bash
# 前端热改：从磁盘读 web 目录，改完刷新即可，无需重新编译
go run . -web-dir web

# 全量测试
go test -count=1 ./...

# 竞态检测（需要 CGO 与本地 GCC）
go test -race -count=1 ./...
```

设置了 `AMAP_KEY` 时，测试会额外运行打真实高德上游的集成用例；未设置时自动跳过并注明原因。

## 数据来源

- 天气与空气质量：[Open-Meteo](https://open-meteo.com/)（免费，无需 API Key）
- 城市搜索与定位取名：[高德开放平台](https://lbs.amap.com/)、[和风天气](https://www.qweather.com/)（可选）

生活指数与出行建议由天气数据按规则推算，仅供参考。

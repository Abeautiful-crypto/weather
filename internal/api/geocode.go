package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"weather/internal/amap"
)

// maxGeocodeCandidates 限制一次搜索最多向上下游发起的候选查询数。
// 正常输入只会产生 1~2 个候选；上限用于防止畸形输入放大成上游压力。
const maxGeocodeCandidates = 4

// geocodeResultLimit 与上游 count 参数一致：合并多个候选后也只回 6 条。
const geocodeResultLimit = 6

// adminPrefixes 是中国省级行政区名称（全称与简称），用于把「江苏宿迁」这类"省+市"
// 输入拆成可被上游检索的词。
//
// 拆词只是**追加**候选，原词永远保留，所以误拆不会丢结果——例如「吉林」既是省也是市、
// 「河南」既是省也是黑龙江的一个地名，最坏情况只是多一次上游查询。
var adminPrefixes = []string{
	"内蒙古自治区", "广西壮族自治区", "西藏自治区", "宁夏回族自治区", "新疆维吾尔自治区",
	"香港特别行政区", "澳门特别行政区",
	"北京市", "天津市", "河北省", "山西省", "辽宁省", "吉林省", "黑龙江省", "上海市",
	"江苏省", "浙江省", "安徽省", "福建省", "江西省", "山东省", "河南省", "湖北省",
	"湖南省", "广东省", "海南省", "重庆市", "四川省", "贵州省", "云南省", "陕西省",
	"甘肃省", "青海省", "台湾省",
	"内蒙古", "广西", "西藏", "宁夏", "新疆", "香港", "澳门",
	"北京", "天津", "河北", "山西", "辽宁", "吉林", "黑龙江", "上海", "江苏", "浙江",
	"安徽", "福建", "江西", "山东", "河南", "湖北", "湖南", "广东", "海南", "重庆",
	"四川", "贵州", "云南", "陕西", "甘肃", "青海", "台湾",
}

// citySuffixes 已带这些后缀的词不再追加「市」。
//
// 注意这里**没有**「州」：徐州、苏州、杭州、广州、兰州等大量城市名本身就以「州」结尾，
// 一旦把「州」当作已有后缀，这些城市就永远不会被追加「市」，从而漏掉 PPLA2 的正式条目
// （实测 `徐州` 只出 PPLA3/PPL，加上 `徐州市` 才拿到 PPLA2 的徐州市、人口 125 万）。
var citySuffixes = []string{"市", "区", "县", "旗", "盟", "镇", "乡", "村"}

// geocodeCandidates 返回该关键词应当尝试的上游查询词，按优先级排列、去重、限量。
//
// 上游 Open-Meteo（GeoNames 数据）实测有两个坑，这就是候选策略的由来：
//   - 「省+市」拼接一律 0 条：`江苏宿迁`、`河南南阳` 查不到，拆出 `宿迁`/`南阳` 才有结果；
//   - `市` 后缀不是"加了更好"而是"看情况"：`南阳` 查不到地级市、`南阳市` 才查到；
//     但 `南京`/`苏州` 反过来——`南京市`/`苏州市` 是 0 条。
//
// 所以既保留原词，也追加拆词与加"市"的变体，最后靠 featureRank 排序把真正的城市顶上来。
func geocodeCandidates(q string) []string {
	out := make([]string, 0, maxGeocodeCandidates)
	seen := make(map[string]bool, maxGeocodeCandidates)

	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || len(out) >= maxGeocodeCandidates {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	add(q)
	if rest := trimAdminPrefix(q); rest != "" {
		// 拆出省名后剩下的才是城市名；此时不再给原词加「市」，因为「江苏宿迁市」
		// 这种词上游不可能命中，纯属浪费一次请求。
		add(rest)
		add(trimAdminSuffix(rest))
		add(withCitySuffix(rest))
		return out
	}
	// 去掉行政区后缀的变体很关键：`淅川县` 在上游中文索引里查不到，
	// 但 `淅川` 能命中「淅川(PPLA3 河南/南阳市)」。
	add(trimAdminSuffix(q))
	add(withCitySuffix(q))

	return out
}

// trimAdminSuffix 去掉结尾的行政区后缀（淅川县 → 淅川）。
// 剩余不足两个字时返回 ""，避免产出「唐」这类噪音候选。
func trimAdminSuffix(s string) string {
	runes := []rune(s)
	if len(runes) < 3 {
		return ""
	}
	last := string(runes[len(runes)-1])
	for _, suf := range citySuffixes {
		if last != suf {
			continue
		}
		rest := string(runes[:len(runes)-1])
		if len([]rune(rest)) < 2 {
			return ""
		}
		return rest
	}
	return ""
}

// trimAdminPrefix 剥掉开头的省级名称并返回剩余部分；不匹配、剥完为空、
// 或剩余不足两个字（如「北京路」剥成「路」）时返回 ""，避免制造噪音候选。
func trimAdminPrefix(q string) string {
	qRunes := []rune(q)
	for _, p := range adminPrefixes {
		if len([]rune(p)) >= len(qRunes) {
			continue
		}
		if strings.HasPrefix(q, p) {
			rest := strings.TrimSpace(strings.TrimPrefix(q, p))
			if len([]rune(rest)) < 2 {
				return ""
			}
			return rest
		}
	}
	return ""
}

// withCitySuffix 给未带行政区后缀的词加「市」，否则返回空串表示不需要追加。
func withCitySuffix(s string) string {
	if s == "" {
		return ""
	}
	for _, suf := range citySuffixes {
		if strings.HasSuffix(s, suf) {
			return ""
		}
	}
	return s + "市"
}

// featureRank 把 GeoNames 的 feature_code 映射为排序权重，越小越"像个城市"。
// 上游返回顺序不按行政级别排，`南阳` 的首条就是人口 1.2 万的 PPLA4 村镇。
func featureRank(code string) int {
	switch code {
	case "PPLC": // 国家首都
		return 0
	case "PPLA": // 一级行政区首府（省/直辖市驻地）
		return 1
	case "PPLA2": // 二级行政区首府（地级市驻地）
		return 2
	case "PPLA3":
		return 3
	case "PPLA4":
		return 4
	case "PPLA5":
		return 5
	case "PPL": // 一般居民点
		return 6
	case "PPLX": // 城区/区片
		return 7
	default:
		return 8
	}
}

// placeMeta 只解出排序与去重需要的字段；raw 保留上游原始 JSON，字段不做任何改写。
type placeMeta struct {
	raw         json.RawMessage
	id          int64
	name        string
	featureCode string
	population  int64
}

// searchPlaces 依次尝试候选查询词，合并去重后按"行政级别 + 人口"重排序，
// 返回与上游同构的 {"results":[...]}。
//
// 与上游的差异仅限于 results 的**顺序与来源**（可能来自多个候选查询），
// 元素本身仍是上游返回的原始对象，不做字段改写。
func (s *Server) searchPlaces(ctx context.Context, q string) ([]byte, error) {
	var all []placeMeta
	seen := map[string]bool{}

	for _, cand := range geocodeCandidates(q) {
		params := url.Values{}
		params.Set("name", cand)
		params.Set("count", strconv.Itoa(geocodeResultLimit))
		params.Set("language", "zh")
		params.Set("format", "json")

		body, err := s.up.FetchWithQuery(ctx, s.up.Endpoints().Geocode, params)
		if err != nil {
			// 硬错误（超时/不可达/上游非 2xx）：直接失败，后续候选不会有不同结果。
			return nil, err
		}

		var parsed struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			continue // 该候选返回不可解析，跳过（不因单个候选异常而让整次搜索失败）
		}

		for _, raw := range parsed.Results {
			var meta struct {
				ID          int64  `json:"id"`
				Name        string `json:"name"`
				FeatureCode string `json:"feature_code"`
				Population  int64  `json:"population"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				continue
			}
			key := strconv.FormatInt(meta.ID, 10)
			if meta.ID == 0 {
				key = "name:" + meta.Name
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, placeMeta{
				raw:         raw,
				id:          meta.ID,
				name:        meta.Name,
				featureCode: meta.FeatureCode,
				population:  meta.Population,
			})
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		ri, rj := featureRank(all[i].featureCode), featureRank(all[j].featureCode)
		if ri != rj {
			return ri < rj
		}
		if all[i].population != all[j].population {
			return all[i].population > all[j].population
		}
		return all[i].name < all[j].name
	})

	if len(all) > geocodeResultLimit {
		all = all[:geocodeResultLimit]
	}
	results := make([]json.RawMessage, 0, len(all))
	for _, it := range all {
		results = append(results, it.raw)
	}
	return json.Marshal(map[string]any{"results": annotateLevels(results)})
}

// levelOfFeatureCode 把 GeoNames 的 feature_code 归一化成中文行政级别。
//
// 前端据此给"村镇级/乡镇级"的低置信度结果加标注：中文同名小地名极多
// （例如"镇平"在福建龙岩有一个村镇级同名点），不标注就会被误选成县级行政区。
func levelOfFeatureCode(code string) string {
	switch code {
	case "PPLC":
		return "首都"
	case "PPLA":
		return "省级"
	case "PPLA2":
		return "地级"
	case "PPLA3":
		return "县级"
	case "PPLA4", "PPLA5":
		return "乡镇级"
	case "PPL":
		return "村镇级"
	case "PPLX":
		return "城区"
	default:
		return ""
	}
}

// annotateLevels 给每个结果**追加**一个归一化的 level 字段。
// 只新增这一个字段，上游原有字段一律不改写、不删除；解析失败的条目原样返回。
func annotateLevels(results []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(results))
	for _, raw := range results {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			out = append(out, raw)
			continue
		}
		code, _ := obj["feature_code"].(string)
		obj["level"] = levelOfFeatureCode(code)
		encoded, err := json.Marshal(obj)
		if err != nil {
			out = append(out, raw)
			continue
		}
		out = append(out, encoded)
	}
	return out
}

// qweatherPlace 是和风 GeoAPI 结果映射后的形状。
//
// 字段名刻意与前端已经在用的 Open-Meteo 地理编码结构保持一致（name / latitude /
// longitude / admin1 / country），所以切换搜索数据源不需要改动前端一行代码——
// 前端只把 latitude/longitude 原样带进 /api/weather。
type qweatherPlace struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Admin1    string  `json:"admin1"`
	Admin2    string  `json:"admin2"`
	Country   string  `json:"country"`
	Source    string  `json:"source"`
}

// searchPlacesQWeather 调用和风 GeoAPI 并把结果映射成前端已有的结构。
//
// 和风返回的经纬度是字符串（如 "33.94917"），这里解析为数字；无法解析的条目直接丢弃，
// 不让脏数据进前端。查无结果时返回 {"results":[]}，与 Open-Meteo 路径行为一致。
func (s *Server) searchPlacesQWeather(ctx context.Context, q string) ([]byte, error) {
	cities, err := s.qw.LookupCity(ctx, q, geocodeResultLimit)
	if err != nil {
		return nil, err
	}
	results := make([]qweatherPlace, 0, len(cities))
	for _, c := range cities {
		lat, latErr := strconv.ParseFloat(c.Lat, 64)
		lon, lonErr := strconv.ParseFloat(c.Lon, 64)
		if latErr != nil || lonErr != nil {
			continue
		}
		results = append(results, qweatherPlace{
			Name:      c.Name,
			Latitude:  lat,
			Longitude: lon,
			Admin1:    c.Adm1,
			Admin2:    c.Adm2,
			Country:   c.Country,
			Source:    "qweather",
		})
	}
	return json.Marshal(map[string]any{"results": results})
}

// amapPlace 是高德地理编码映射到前端已有结构后的形状。
//
// 字段名与另外两条路径（Open-Meteo / 和风）保持一致，前端因此不需要知道
// 当前用的是哪个数据源。
type amapPlace struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Admin1    string  `json:"admin1"`
	Country   string  `json:"country"`
	Level     string  `json:"level"`
	Adcode    string  `json:"adcode"`
	Source    string  `json:"source"`
}

// searchPlacesAMap 调用高德地理编码并把结果映射成前端已有结构。
//
// 命名规则是为了让实测的三条「朝阳」结果**仅凭 name + admin1 就能区分**
// （前端只展示这两个字段）：
//   - 有区县时 name 取区县名、admin1 取「省 · 市」——于是"北京市朝阳区"读作
//     「朝阳区 | 北京市 · 中国」、"长春市朝阳区"读作「朝阳区 | 吉林省 · 长春市 · 中国」；
//   - 无区县时 name 取市名、admin1 取省名，直辖市（省==市）时 admin1 置空，
//     由前端的既有去重逻辑渲染成「上海市 | 中国」。
//
// 刻意不做 level 白名单过滤：实测输入城市名时高德只返回行政级别结果
// （"朝阳"返回 3 条，全部是市/区县），丢弃逻辑没有收益却可能把有效结果滤掉。
// 非行政级别的匹配（道路、兴趣点、门牌号）交由 level 标注提示用户，
// 而不是让结果凭空消失。
func (s *Server) searchPlacesAMap(ctx context.Context, q string) ([]byte, error) {
	places, err := s.am.Geocode(ctx, q)
	if errors.Is(err, amap.ErrNoResult) {
		// 高德明确"这个输入解析不出来"（实测 infocode=30001），语义等同于"没有匹配"：
		// 返回空结果让回落链继续，前端最终会显示「未找到相关城市」。
		return emptyResults(), nil
	}
	if err != nil {
		return nil, err
	}

	results := make([]amapPlace, 0, len(places))
	for _, p := range places {
		var name, admin1 string
		switch {
		case p.District != "":
			name = p.District
			admin1 = joinUnique(p.Province, p.City)
		case p.City != "":
			name = p.City
			if p.Province != name {
				admin1 = p.Province
			}
		default:
			name = firstNonEmpty(p.Province, p.Name)
		}

		results = append(results, amapPlace{
			Name:      name,
			Latitude:  p.Latitude,
			Longitude: p.Longitude,
			Admin1:    admin1,
			Country:   p.Country,
			Level:     levelOfAMapLevel(p.Level),
			Adcode:    p.Adcode,
			Source:    "amap",
		})
	}
	return json.Marshal(map[string]any{"results": results})
}

// levelOfAMapLevel 把高德的匹配级别归一化成与 GeoNames 路径同一套中文词汇，
// 让前端那套"低置信度标注"对高德结果同样生效。
//
// 高德的 level 描述的是"地址解析匹配到哪一级"，因此会出现行政区之外的取值
// （道路、兴趣点、门牌号、公交站点…）。这些值没有行政含义，统一标注为
// 「非行政地名」，避免用户把它当成正常城市候选选中。
func levelOfAMapLevel(level string) string {
	switch strings.TrimSpace(level) {
	case "", "国家":
		return ""
	case "省":
		return "省级"
	case "市":
		return "地级"
	case "区县", "开发区":
		return "县级"
	case "乡镇":
		return "乡镇级"
	case "村庄":
		return "村镇级"
	default:
		return "非行政地名"
	}
}

// joinUnique 用「 · 」连接非空且互不相同的部分，避免直辖市出现
// 「北京市 · 北京市」这种重复展示。
func joinUnique(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		dup := false
		for _, seen := range out {
			if seen == p {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// firstNonEmpty 返回第一个非空（去空白后）的字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// resultCount 统计 {"results":[...]} 里的条目数，用于判断"这一路是否查无结果"。
// 无法解析时返回 0；正常路径的 body 都由本包自己序列化，不存在这种情况。
func resultCount(body []byte) int {
	var parsed struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0
	}
	return len(parsed.Results)
}

// emptyResults 返回 {"results":[]}。必须是数组而不是 null，前端才能走到
// "未找到相关城市"分支。
func emptyResults() []byte { return []byte(`{"results":[]}`) }

// administrativeLevels 是"算得上行政区"的归一化级别，两套词汇表（GeoNames 与高德）
// 都列在这里，改词表时两边一起改。
var administrativeLevels = map[string]bool{
	"首都": true, "省级": true, "地级": true, "县级": true, "乡镇级": true, "城区": true,
}

// hasAdministrativeMatch 判断这一路的结果里是否至少有一条落到了行政区级别。
//
// 高德是"地址解析"而不是"地名搜索"：解析不出目标地名时它会返回一堆字面同名的
// 低级别结果（实测 东京 / Tokyo → 6 条 level=村庄 的「…平南县东京」）。这类"弱结果"
// 若被当成"有结果"，就会永久挡住后面数据源的正确答案（GeoNames 的东京）。
//
// 弱结果也不能直接丢：搜"三元村"时高德给出 10 条真实存在的同名村庄，那就是这个
// 输入的全部答案，所以调用方只在"没有更强结果"时才拿它兜底。
func hasAdministrativeMatch(body []byte) bool {
	var parsed struct {
		Results []struct {
			Level string `json:"level"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	for _, r := range parsed.Results {
		if administrativeLevels[r.Level] {
			return true
		}
	}
	return false
}

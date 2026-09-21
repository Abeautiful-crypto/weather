package api

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
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

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"weather/internal/amap"
)

// errRegeoNoResult 表示高德如实回答"这个坐标没有对应的地区数据"（境外或海上）。
// 它既不是客户端错误，也不是上游故障，因此单独区分出来。
var errRegeoNoResult = errors.New("该坐标没有对应的地区数据")

// translateNoResult 把 amap.ErrNoResult 归一成 api 层的 errRegeoNoResult。
//
// 定位取名有两次上游调用（坐标转换、逆地理编码），任何一步都可能给出 ErrNoResult。
// 在入口统一转换才不会有漏网的那一步退化成 500「服务内部错误」。
func translateNoResult(err error) error {
	if errors.Is(err, amap.ErrNoResult) {
		return errRegeoNoResult
	}
	return err
}

// regeoView 是 /api/regeo 的响应体。
//
// 刻意不返回坐标：天气查询必须继续使用原始 GPS 坐标（Open-Meteo 要求 WGS-84），
// 而本接口内部为了取到正确区县必须先把坐标转成高德坐标系（GCJ-02）。两套坐标
// 各司其职，把转换后的坐标暴露出去只会诱导调用方误用。
type regeoView struct {
	Name     string `json:"name"`
	Province string `json:"province"`
	City     string `json:"city"`
	District string `json:"district"`
	Adcode   string `json:"adcode"`
	Country  string `json:"country"`
	Source   string `json:"source"`
}

// handleRegeo 把 GPS 坐标解析成行政区名，供前端"定位"流程显示真实城市名与区县。
//
// 未配置 AMAP_KEY 时返回 503：配置缺失既不是客户端错误（400），也不是上游故障
// （502/504）。用 503 才能让调用方区分"这个能力没开"与"这次调用失败了"，前端
// 据此回落成"当前位置 + 经纬度"的原有展示。
func (s *Server) handleRegeo(w http.ResponseWriter, r *http.Request) {
	if s.am == nil {
		writeError(w, http.StatusServiceUnavailable, "未启用定位取名（服务端未配置 AMAP_KEY），将只显示经纬度")
		return
	}
	lat, lon, errMsg := parseCoord(r)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	key := "regeo:" + formatCoord(lat) + ":" + formatCoord(lon)
	data, err, hit := s.cache.GetOrLoad(key, func() ([]byte, bool, error) {
		// 与其它接口一致：上游调用脱离客户端 context，避免断线连坐单飞等待者。
		ctx := context.WithoutCancel(r.Context())
		body, lookupErr := s.lookupDistrict(ctx, lat, lon)
		if lookupErr != nil {
			return nil, false, lookupErr
		}
		return body, true, nil
	})
	if err != nil {
		if errors.Is(err, errRegeoNoResult) {
			// 既不是 400（请求没问题）也不是 502（服务没坏）：高德确实"不知道这里是哪"。
			// 前端对任何非 2xx 都回落到"当前位置 + 经纬度"，所以 404 足够表达。
			writeError(w, http.StatusNotFound, "未能识别该位置所属地区（可能位于境外或海上），请手动搜索城市")
			return
		}
		s.writeProxyError(w, "regeo", err)
		return
	}
	s.writeCachedJSON(w, "regeo", key, data, hit)
}

// lookupDistrict 先做 GPS → 高德坐标转换，再逆地理编码。
//
// 这一步不能省：逆地理编码要求高德坐标系，直接把 GPS 的 WGS-84 喂进去会偏约
// 400 米，实测足以让"天安门"从东城区变成西城区（东城/西城分界正好穿过那里）。
func (s *Server) lookupDistrict(ctx context.Context, lat, lon float64) ([]byte, error) {
	amLat, amLon, err := s.am.GPSToAMap(ctx, lat, lon)
	if err != nil {
		return nil, translateNoResult(err)
	}
	place, err := s.am.Regeo(ctx, amLat, amLon)
	if err != nil {
		return nil, translateNoResult(err)
	}
	return json.Marshal(regeoView{
		Name:     place.Name,
		Province: place.Province,
		City:     place.City,
		District: place.District,
		Adcode:   place.Adcode,
		Country:  place.Country,
		Source:   "amap",
	})
}

// Package location provides related functionality.
package location

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/pkg/limiter"
	"papafeiji/backend/pkg/util"
)

type Client struct {
	apiKeys []string
	index   uint64
	client  *http.Client
}

func NewClient(apiKeys []string) *Client {
	return &Client{
		apiKeys: apiKeys,
		client:  config.HTTPClient(),
	}
}

type ReverseResult struct {
	Address       string
	DetailAddress string
	Landmark      string
	AreaCode      string
	AreaName      string
	POIs          []POI
}

type POI struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Category string  `json:"category"`
	Distance float64 `json:"distance"`
	Address  string  `json:"address"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
}

func (c *Client) NextKey() string {
	n := len(c.apiKeys)
	if n == 0 {
		return ""
	}
	idx := atomic.AddUint64(&c.index, 1) - 1
	return c.apiKeys[idx%uint64(n)]
}

func (c *Client) Reverse(ctx context.Context, lat, lon float64, withPois bool) (*ReverseResult, error) {
	// 腾讯地图 key 未配置时短路返回明确错误，避免空 key 请求后慢失败 500。
	if len(c.apiKeys) == 0 {
		return nil, fmt.Errorf("tencent map key not configured")
	}
	apiKey := c.NextKey()
	u := "https://apis.map.qq.com/ws/geocoder/v1/"

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			apiKey = c.NextKey()
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		if err := limiter.WaitGeoCoder(ctx); err != nil {
			return nil, fmt.Errorf("geocoder rate limiter wait: %w", err)
		}

		params := url.Values{}
		params.Set("key", apiKey)
		params.Set("location", fmt.Sprintf("%f,%f", lat, lon))
		if withPois {
			params.Set("get_poi", "1")
			params.Set("poi_options", "radius=500;page_size=10;page_index=1;policy=2")
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
		if err != nil {
			return nil, err
		}

		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := c.client.Do(req.WithContext(callCtx))
		if err != nil {
			cancel()
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close() //nolint:errcheck
			cancel()
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}

		var payload struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
			Result  struct {
				Address            string `json:"address"`
				AddressTitle       string `json:"title"`
				FormattedAddresses struct {
					Recommend string `json:"recommend"`
					Rough     string `json:"rough"`
				} `json:"formatted_addresses"`
				Pois []struct {
					ID       string  `json:"id"`
					Title    string  `json:"title"`
					Category string  `json:"category"`
					Distance float64 `json:"_distance"`
					Address  string  `json:"address"`
					Location struct {
						Lat float64 `json:"lat"`
						Lng float64 `json:"lng"`
					} `json:"location"`
				} `json:"pois"`
				AdInfo struct {
					Adcode   string `json:"adcode"`
					District string `json:"district"`
					Name     string `json:"name"`
				} `json:"ad_info"`
			} `json:"result"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload)
		resp.Body.Close() //nolint:errcheck
		cancel()
		if decodeErr != nil {
			lastErr = decodeErr
			continue
		}

		if payload.Status != 0 {
			lastErr = fmt.Errorf("tencent error: %s", payload.Message)
			continue
		}

		address := payload.Result.Address
		detail := payload.Result.FormattedAddresses.Recommend
		if detail == "" {
			detail = payload.Result.FormattedAddresses.Rough
		}
		if detail == "" {
			detail = payload.Result.AddressTitle
		}

		landmark := payload.Result.AddressTitle
		if landmark == "" {
			if len(payload.Result.Pois) > 0 {
				landmark = payload.Result.Pois[0].Title
			}
			if landmark == "" {
				landmark = detail
			}
		}

		var pois []POI
		for _, p := range payload.Result.Pois {
			pois = append(pois, POI{
				ID:       p.ID,
				Title:    p.Title,
				Category: p.Category,
				Distance: p.Distance,
				Address:  p.Address,
				Lat:      p.Location.Lat,
				Lng:      p.Location.Lng,
			})
		}

		areaCode := payload.Result.AdInfo.Adcode
		areaName := payload.Result.AdInfo.District
		if areaName == "" {
			areaName = payload.Result.AdInfo.Name
		}
		return &ReverseResult{
			Address:       address,
			DetailAddress: detail,
			Landmark:      landmark,
			AreaCode:      areaCode,
			AreaName:      areaName,
			POIs:          pois,
		}, nil
	}
	return nil, util.SanitizeURLError(fmt.Errorf("reverse geocode failed: %w", lastErr))
}

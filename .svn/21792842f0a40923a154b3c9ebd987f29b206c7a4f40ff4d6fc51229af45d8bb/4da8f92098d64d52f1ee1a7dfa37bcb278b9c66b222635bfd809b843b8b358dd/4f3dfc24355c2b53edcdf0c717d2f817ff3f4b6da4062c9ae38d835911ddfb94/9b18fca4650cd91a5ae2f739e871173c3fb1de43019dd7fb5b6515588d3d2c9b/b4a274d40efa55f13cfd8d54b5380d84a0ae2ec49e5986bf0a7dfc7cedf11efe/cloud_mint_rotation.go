package main

import (
	"strings"
	"sync"
	"time"
)

// cloudEndpointCooldown 是"某个云函数地址刚铸不出票"之后跳过多长时间。
// 依据仓库笔记：每个号有满血票冷却时间，拿完了就不能拿了，轮换服务端即可解决。
const cloudEndpointCooldown = 90 * time.Second

var cloudEndpointRotation = struct {
	mu       sync.Mutex
	next     int
	cooldown map[string]time.Time
}{cooldown: make(map[string]time.Time)}

// cloudEndpointLabel 给日志用的短标签：只留主机名，不把整条地址铺进日志。
func cloudEndpointLabel(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "未配置地址"
	}
	withoutScheme := trimmed
	if index := strings.Index(withoutScheme, "://"); index >= 0 {
		withoutScheme = withoutScheme[index+3:]
	}
	if index := strings.IndexAny(withoutScheme, "/?#"); index >= 0 {
		withoutScheme = withoutScheme[:index]
	}
	if withoutScheme == "" {
		return "未配置地址"
	}
	return withoutScheme
}

// pickCloudEndpoint 从 next 开始顺序挑一个没在冷却的地址；全都冷却时返回最早恢复的那个
// （总比一次都不打强），空列表返回空串。
func pickCloudEndpoint(endpoints []string, now time.Time) string {
	if len(endpoints) == 0 {
		return ""
	}
	cloudEndpointRotation.mu.Lock()
	defer cloudEndpointRotation.mu.Unlock()
	start := cloudEndpointRotation.next % len(endpoints)
	for offset := 0; offset < len(endpoints); offset++ {
		endpoint := endpoints[(start+offset)%len(endpoints)]
		if until, cooling := cloudEndpointRotation.cooldown[endpoint]; !cooling || !until.After(now) {
			return endpoint
		}
	}
	best := endpoints[start]
	bestUntil := cloudEndpointRotation.cooldown[best]
	for _, endpoint := range endpoints {
		if until := cloudEndpointRotation.cooldown[endpoint]; until.Before(bestUntil) {
			best, bestUntil = endpoint, until
		}
	}
	return best
}

// markCloudEndpointFailed 记下这个地址刚失败：进入冷却，并把下次首选项挪到它的后一个地址。
func markCloudEndpointFailed(endpoints []string, endpoint string, now time.Time, reason string) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return
	}
	cloudEndpointRotation.mu.Lock()
	cloudEndpointRotation.cooldown[endpoint] = now.Add(cloudEndpointCooldown)
	for index, candidate := range endpoints {
		if candidate == endpoint {
			cloudEndpointRotation.next = (index + 1) % len(endpoints)
			break
		}
	}
	cloudEndpointRotation.mu.Unlock()
	if len(endpoints) > 1 {
		cloudRecordLog("换云函数地址", "%s 铸票失败，暂时跳过它（%ds）%s", cloudEndpointLabel(endpoint), int(cloudEndpointCooldown/time.Second), cloudReasonSuffix(reason))
	}
}

// markCloudEndpointOK 铸票成功：清掉这个地址的冷却，下次继续用它。
func markCloudEndpointOK(endpoint string) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return
	}
	cloudEndpointRotation.mu.Lock()
	delete(cloudEndpointRotation.cooldown, endpoint)
	cloudEndpointRotation.mu.Unlock()
}

// cloudReasonSuffix 把失败原因拼成日志后缀；空原因就不拼。
func cloudReasonSuffix(reason string) string {
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		return " · " + cloudSafeLabel(trimmed)
	}
	return ""
}

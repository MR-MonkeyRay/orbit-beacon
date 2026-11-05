// orbitbeacon-load 仅通过公开的 Agent HTTP 接口生成开发负载，不参与发布。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	randv2 "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "orbitbeacon/internal/protocol/v1"
)

type tickets []string

func (t *tickets) String() string { return "<票据已隐藏>" }
func (t *tickets) Set(value string) error {
	if value = strings.TrimSpace(value); value == "" {
		return errors.New("票据不能为空")
	}
	*t = append(*t, value)
	return nil
}

type options struct {
	baseURL                        string
	nodes, disks, networks, probes int
	interval, duration, jitter     time.Duration
	tickets                        tickets
	ticketFile                     string
}

type node struct {
	token, installation, session, epoch string
	system                              v1.System
}
type outcome struct {
	status  int
	code    string
	elapsed time.Duration
}
type summary struct {
	mu        sync.Mutex
	outcomes  map[string]int
	latencies []time.Duration
	success   int
	buckets   []reportBucket
}

const summaryBucketInterval = 10 * time.Second

type reportBucket struct {
	reports, busy int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.baseURL, "url", "http://127.0.0.1:8080", "服务器根 URL")
	flag.IntVar(&o.nodes, "nodes", 200, "模拟节点数")
	flag.DurationVar(&o.interval, "interval", 10*time.Second, "上报间隔（运行时声明限制为 5–60s）")
	flag.DurationVar(&o.duration, "duration", 5*time.Minute, "注册完成后的施压时长")
	flag.DurationVar(&o.jitter, "jitter", 0, "每次计划发送时间的随机抖动 ±时长（小于间隔的一半）")
	flag.IntVar(&o.disks, "disks", 1, "每节点磁盘数，0–8")
	flag.IntVar(&o.networks, "networks", 1, "每节点网卡数，0–8")
	flag.IntVar(&o.probes, "probes", 0, "每节点合成探测目标数，0–5；不会实际访问目标")
	flag.Var(&o.tickets, "ticket", "批量注册票据，可重复指定，每张最多注册 100 个节点")
	flag.StringVar(&o.ticketFile, "tickets-file", "", "票据文件，每行一张")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("不接受位置参数")
	}
	if err := o.validate(); err != nil {
		return err
	}
	o.baseURL = strings.TrimRight(o.baseURL, "/")
	if o.ticketFile != "" {
		f, err := os.Open(o.ticketFile)
		if err != nil {
			return errors.New("无法读取票据文件")
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if value := strings.TrimSpace(scanner.Text()); value != "" {
				o.tickets = append(o.tickets, value)
			}
		}
		err = scanner.Err()
		_ = f.Close()
		if err != nil {
			return errors.New("无法解析票据文件")
		}
	}
	seen := make(map[string]bool)
	unique := tickets{}
	for _, ticket := range o.tickets {
		if !seen[ticket] {
			unique = append(unique, ticket)
			seen[ticket] = true
		}
	}
	o.tickets = unique
	if len(o.tickets) < (o.nodes-1)/100+1 {
		return fmt.Errorf("%d 个节点至少需要 %d 张不同的批量票据", o.nodes, (o.nodes-1)/100+1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	stats := &summary{outcomes: make(map[string]int)}
	defer stats.print()
	nodes := make([]node, 0, o.nodes)
	ticketIndex, uses := 0, 0
	for i := range o.nodes {
		if ctx.Err() != nil {
			return nil
		}
		n, err := newNode(i)
		if err != nil {
			return err
		}
		input := v1.EnrollmentRequest{ProtocolVersion: v1.ProtocolVersion, InstallationID: n.installation, NodeToken: n.token, System: n.system}
		for {
			if ticketIndex >= len(o.tickets) {
				return fmt.Errorf("批量票据剩余配额不足，已注册 %d 个节点", len(nodes))
			}
			result, body, err := post(ctx, client, o.baseURL+"/api/agent/v1/enroll", o.tickets[ticketIndex], input)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("节点 %d 注册传输失败，已注册 %d 个节点", i+1, len(nodes))
			}
			if result.status == http.StatusConflict && result.code == "enrollment_conflict" {
				ticketIndex++
				uses = 0
				continue
			}
			if result.status != http.StatusOK {
				return fmt.Errorf("节点 %d 注册失败：HTTP %d（已注册 %d 个节点）", i+1, result.status, len(nodes))
			}
			var enrolled v1.EnrollmentResponse
			if json.Unmarshal(body, &enrolled) != nil || enrolled.NodeID == "" {
				return errors.New("注册响应无效")
			}
			nodes = append(nodes, n)
			uses++
			if uses == 100 {
				ticketIndex++
				uses = 0
			}
			break
		}
	}
	fmt.Fprintf(os.Stderr, "已注册 %d 个模拟节点，开始施压 %s；退出后节点保留在服务器。\n", len(nodes), o.duration)
	loadCtx, cancel := context.WithTimeout(ctx, o.duration)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func() { defer wg.Done(); sendReports(loadCtx, client, o, n, start, stats) }()
	}
	wg.Wait()
	return nil
}

func (o options) validate() error {
	u, err := url.Parse(o.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("url 必须是无凭据、查询和路径的 HTTP(S) 根地址")
	}
	if o.nodes < 1 || o.duration <= 0 || o.interval < 5*time.Second || o.interval > 60*time.Second || o.interval%time.Second != 0 || o.jitter < 0 || o.jitter >= o.interval/2 {
		return errors.New("节点数和时长必须为正；间隔须为 5–60 整秒；抖动须非负且小于间隔的一半")
	}
	if o.disks < 0 || o.disks > 8 || o.networks < 0 || o.networks > 8 || o.probes < 0 || o.probes > 5 {
		return errors.New("磁盘和网卡数须为 0–8，探测目标数须为 0–5")
	}
	return nil
}

func newNode(index int) (node, error) {
	values := make([]string, 4)
	for i := range values {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return node{}, errors.New("无法生成模拟节点身份")
		}
		values[i] = hex.EncodeToString(b[:])
	}
	return node{token: values[0], installation: values[1], session: values[2], epoch: values[3], system: v1.System{Hostname: fmt.Sprintf("load-%s-%d", values[1][:8], index+1), OS: "linux", Arch: "amd64", AgentVersion: "load-development"}}, nil
}

// 每节点最多一个在途请求；慢请求跨过的计划时刻会跳过，避免客户端无限积压。
func sendReports(ctx context.Context, client *http.Client, o options, n node, start time.Time, stats *summary) {
	base := start.Add(o.jitter)
	for seq := uint64(1); ; seq++ {
		scheduled := base
		if o.jitter > 0 {
			scheduled = scheduled.Add(time.Duration(randv2.Int64N(int64(2*o.jitter)+1)) - o.jitter)
		}
		timer := time.NewTimer(time.Until(scheduled))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		report := makeReport(o, n, seq, started.UTC())
		result, _, _ := post(ctx, client, strings.TrimRight(o.baseURL, "/")+"/api/agent/v1/report", n.token, report)
		// 结束时被取消的在途请求不是服务器失败，不计入汇总。
		if ctx.Err() != nil && result.status == 0 {
			return
		}
		stats.record(result, started.Sub(start))
		base = base.Add(o.interval)
		if missed := time.Since(base); missed > 0 {
			base = base.Add((missed/o.interval + 1) * o.interval)
		}
	}
}

func post(ctx context.Context, client *http.Client, target, token string, input any) (outcome, []byte, error) {
	started := time.Now()
	result := outcome{}
	body, err := json.Marshal(input)
	if err != nil || len(body) > v1.MaxMessageBytes {
		return result, nil, errors.New("请求编码失败或超过协议预算")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return result, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		result.elapsed = time.Since(started)
		return result, nil, err
	}
	defer response.Body.Close()
	result.status = response.StatusCode
	data, err := io.ReadAll(io.LimitReader(response.Body, v1.MaxMessageBytes+1))
	result.elapsed = time.Since(started)
	if err != nil || len(data) > v1.MaxMessageBytes {
		result.code = "response_error"
		return result, nil, errors.New("响应读取失败或过大")
	}
	var failure struct {
		Code string `json:"code"`
	}
	if response.StatusCode >= 400 && json.Unmarshal(data, &failure) == nil && failure.Code == "report_busy" {
		result.code = failure.Code
	}
	// 注册冲突仅用于尝试下一张票据；汇总不接收任意服务端标签。
	if response.StatusCode == 409 && failure.Code == "enrollment_conflict" {
		result.code = failure.Code
	}
	return result, data, nil
}

func (s *summary) record(result outcome, sinceStart time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%d", result.status)
	if result.status == 0 {
		key = "transport_error"
	}
	if result.code != "" {
		key += " " + result.code
	}
	s.outcomes[key]++
	if result.status >= 200 && result.status < 300 && result.code == "" {
		s.success++
	}
	s.latencies = append(s.latencies, result.elapsed)
	// 按请求开始时间分段，避免慢响应把同一轮上报挪到下一段。
	index := int(sinceStart / summaryBucketInterval)
	for len(s.buckets) <= index {
		s.buckets = append(s.buckets, reportBucket{})
	}
	s.buckets[index].reports++
	if result.code == "report_busy" {
		s.buckets[index].busy++
	}
}

func (s *summary) print() {
	s.printTo(os.Stdout)
}

func (s *summary) printTo(w io.Writer) {
	sort.Slice(s.latencies, func(i, j int) bool { return s.latencies[i] < s.latencies[j] })
	rate := 0.0
	if len(s.latencies) > 0 {
		rate = 100 * float64(s.success) / float64(len(s.latencies))
	}
	fmt.Fprintf(w, "上报次数：%d，成功：%d，成功率：%.2f%%\n", len(s.latencies), s.success, rate)
	keys := make([]string, 0, len(s.outcomes))
	for key := range s.outcomes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(w, "  %s: %d\n", key, s.outcomes[key])
	}
	fmt.Fprintf(w, "延迟（全部已完成上报，含失败）：p50=%s p95=%s p99=%s\n", percentile(s.latencies, 0.50), percentile(s.latencies, 0.95), percentile(s.latencies, 0.99))
	for i, bucket := range s.buckets {
		fmt.Fprintf(w, "  %s–%s: reports=%d report_busy=%d\n", time.Duration(i)*summaryBucketInterval, time.Duration(i+1)*summaryBucketInterval, bucket.reports, bucket.busy)
	}
}
func percentile(sorted []time.Duration, quantile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(math.Ceil(float64(len(sorted))*quantile))-1]
}

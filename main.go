package main

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const (
	maxConcurrent            = 100
	maxConcurrentPortScanIPs = 10
	maxConcurrentPortsPerIP  = 20
	// Pause between TCP probes on the same host (OpenWrt/gateways often rate-limit burst SYNs).
	interPortProbeDelay = 0
)

// formatIntsForLog returns a short string for logging large port lists.
func formatIntsForLog(values []int, head int) string {
	if len(values) == 0 {
		return "[]"
	}
	if len(values) <= head {
		return fmt.Sprintf("%v", values)
	}
	return fmt.Sprintf("len=%d first=%v … last=%d", len(values), values[:head], values[len(values)-1])
}

func formatOpenPorts(open []PortInfo) string {
	if len(open) == 0 {
		return "(none)"
	}
	parts := make([]string, len(open))
	for i, p := range open {
		parts[i] = fmt.Sprintf("%d/%s", p.Port, p.Service)
	}
	return strings.Join(parts, ", ")
}

type PortInfo struct {
	Port    int
	Service string
}

type IPInfo struct {
	IP    string
	Ports []PortInfo
}

type Scanner struct {
	mu        sync.Mutex
	onlineIPs []string
	ipInfoMap map[string]*IPInfo
	scanning  bool
	stopFlag  bool
	wg        sync.WaitGroup
}

var commonPorts = map[int]string{
	21:    "FTP",
	22:    "SSH",
	23:    "Telnet",
	25:    "SMTP",
	53:    "DNS",
	80:    "HTTP",
	110:   "POP3",
	135:   "RPC",
	139:   "NetBIOS",
	143:   "IMAP",
	443:   "HTTPS",
	445:   "SMB",
	993:   "IMAPS",
	995:   "POP3S",
	1433:  "MSSQL",
	1521:  "Oracle",
	3306:  "MySQL",
	3389:  "RDP",
	5001:  "Flask",
	5432:  "PostgreSQL",
	5900:  "VNC",
	6379:  "Redis",
	8080:  "HTTP-Alt",
	8443:  "HTTPS-Alt",
	27017: "MongoDB",
}

func NewScanner() *Scanner {
	return &Scanner{
		onlineIPs: make([]string, 0),
		ipInfoMap: make(map[string]*IPInfo),
	}
}

func (s *Scanner) getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "192.168.1.0/24"
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	ipParts := strings.Split(localAddr.IP.String(), ".")
	if len(ipParts) == 4 {
		return fmt.Sprintf("%s.%s.%s.0/24", ipParts[0], ipParts[1], ipParts[2])
	}
	return "192.168.1.0/24"
}

func (s *Scanner) pingIP(ip string) bool {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "1", "-w", "1000", ip)
	} else {
		cmd = exec.Command("ping", "-c", "1", "-W", "1", ip)
	}

	output, err := cmd.CombinedOutput()
	outputStr := strings.ToLower(string(output))

	log.Printf("[ping] %s: output=%q, err=%v", ip, outputStr, err)

	if strings.Contains(outputStr, "ttl") {
		log.Printf("[ping] %s: matches 'ttl' -> online", ip)
		return true
	}

	if strings.Contains(outputStr, "bytes from") {
		log.Printf("[ping] %s: matches 'bytes from' -> online", ip)
		return true
	}

	if strings.Contains(outputStr, "1 packets received") {
		log.Printf("[ping] %s: matches '1 packets received' -> online", ip)
		return true
	}

	if strings.Contains(outputStr, "1 packets transmitted, 1 packets received") {
		log.Printf("[ping] %s: matches full success -> online", ip)
		return true
	}

	if err == nil {
		log.Printf("[ping] %s: command success -> online", ip)
		return true
	}

	log.Printf("[ping] %s: no match, offline", ip)
	return false
}

func (s *Scanner) scanPort(ip string, port int, timeout time.Duration) bool {
	target := fmt.Sprintf("%s:%d", ip, port)
	conn, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		log.Printf("[port] %s:%d: dial failed, err=%v", ip, port, err)
		return false
	}
	conn.Close()
	log.Printf("[port] %s:%d: OPEN!", ip, port)
	return true
}

func (s *Scanner) scanPortsForIP(ip string, ports []int, timeout time.Duration) []PortInfo {
	var openPorts []PortInfo
	var portMu sync.Mutex
	portSem := make(chan struct{}, maxConcurrentPortsPerIP)
	var portWg sync.WaitGroup

	for _, port := range ports {
		s.mu.Lock()
		shouldStop := s.stopFlag
		s.mu.Unlock()

		if shouldStop {
			break
		}

		portWg.Add(1)
		portSem <- struct{}{}

		go func(p int) {
			defer portWg.Done()
			defer func() { <-portSem }()

			if s.scanPort(ip, p, timeout) {
				service := commonPorts[p]
				if service == "" {
					service = "Unknown"
				}
				portMu.Lock()
				openPorts = append(openPorts, PortInfo{Port: p, Service: service})
				portMu.Unlock()
			}
		}(port)
	}

	portWg.Wait()

	if len(openPorts) > 0 {
		sort.Slice(openPorts, func(i, j int) bool {
			return openPorts[i].Port < openPorts[j].Port
		})
	}
	return openPorts
}

func (s *Scanner) scanNetwork(network string, resultChan chan<- string, progressChan chan<- int, doneChan chan<- bool) {
	s.mu.Lock()
	s.onlineIPs = make([]string, 0)
	s.ipInfoMap = make(map[string]*IPInfo)
	s.scanning = true
	s.stopFlag = false
	s.mu.Unlock()

	ipParts := strings.Split(network, ".")
	if len(ipParts) < 3 {
		close(resultChan)
		close(progressChan)
		doneChan <- true
		return
	}

	prefix := fmt.Sprintf("%s.%s.%s", ipParts[0], ipParts[1], ipParts[2])
	semaphore := make(chan struct{}, maxConcurrent)

	for i := 1; i <= 254; i++ {
		s.mu.Lock()
		shouldStop := s.stopFlag
		s.mu.Unlock()

		if shouldStop {
			break
		}

		s.wg.Add(1)
		semaphore <- struct{}{}

		go func(ip string, idx int) {
			defer s.wg.Done()
			defer func() { <-semaphore }()

			if s.pingIP(ip) {
				select {
				case resultChan <- ip:
				default:
				}
			}
			select {
			case progressChan <- idx:
			default:
			}
		}(fmt.Sprintf("%s.%d", prefix, i), i)
	}

	s.wg.Wait()
	close(resultChan)
	close(progressChan)

	s.mu.Lock()
	s.scanning = false
	s.mu.Unlock()

	doneChan <- true
}

func (s *Scanner) scanAllPorts(ports []int, timeout time.Duration, portResultChan chan<- string) {
	s.mu.Lock()
	s.scanning = true
	s.stopFlag = false
	ips := make([]string, len(s.onlineIPs))
	copy(ips, s.onlineIPs)
	s.mu.Unlock()

	if len(ips) == 0 {
		log.Printf("[portscan] skip: no online IPs to scan")
		close(portResultChan)
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
		return
	}

	log.Printf("[portscan] start hosts=%v | each_host_probe_ports=%s | concurrent_hosts=%d inter_port_delay=%v dial_timeout=%v",
		ips, formatIntsForLog(ports, 80), maxConcurrentPortScanIPs, interPortProbeDelay, timeout)

	semaphore := make(chan struct{}, maxConcurrentPortScanIPs)

	for _, ip := range ips {
		s.mu.Lock()
		shouldStop := s.stopFlag
		s.mu.Unlock()

		if shouldStop {
			log.Printf("[portscan] stop: abort launching workers before all hosts were scheduled")
			break
		}

		s.wg.Add(1)
		semaphore <- struct{}{}

		go func(targetIP string) {
			defer s.wg.Done()
			defer func() { <-semaphore }()

			probeList := formatIntsForLog(ports, 80)
			log.Printf("[portscan] host %s begin | probe_ports=%s | port_count=%d",
				targetIP, probeList, len(ports))

			openPorts := s.scanPortsForIP(targetIP, ports, timeout)

			s.mu.Lock()
			s.ipInfoMap[targetIP] = &IPInfo{IP: targetIP, Ports: openPorts}
			s.mu.Unlock()

			log.Printf("[portscan] host %s done | probe_ports=%s | open_ports=%s",
				targetIP, probeList, formatOpenPorts(openPorts))

			// Blocking send: non-blocking + default could drop UI updates; buffer is len(ips).
			portResultChan <- targetIP
		}(ip)
	}

	s.wg.Wait()
	close(portResultChan)

	s.mu.Lock()
	stopped := s.stopFlag
	s.scanning = false
	s.mu.Unlock()
	if stopped {
		log.Printf("[portscan] finished (interrupted or user stopped)")
	} else {
		log.Printf("[portscan] finished all %d host(s)", len(ips))
	}
}

func (s *Scanner) stop() {
	s.mu.Lock()
	s.stopFlag = true
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Scanner) addIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.onlineIPs {
		if existing == ip {
			return
		}
	}
	s.onlineIPs = append(s.onlineIPs, ip)
}

func (s *Scanner) getOnlineIPs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.onlineIPs...)
}

func (s *Scanner) getIPInfo(ip string) *IPInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ipInfoMap[ip]
}

func (s *Scanner) isScanning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanning
}

func parsePorts(portStr string) ([]int, error) {
	var ports []int
	portMap := make(map[int]bool)

	parts := strings.Split(portStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid port range: %s", part)
			}
			start, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			if err != nil {
				return nil, err
			}
			end, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
			if err != nil {
				return nil, err
			}
			if start > end {
				return nil, fmt.Errorf("invalid port range: %s (start > end)", part)
			}
			for p := start; p <= end; p++ {
				if p > 0 && p <= 65535 && !portMap[p] {
					ports = append(ports, p)
					portMap[p] = true
				}
			}
		} else {
			p, err := strconv.Atoi(part)
			if err != nil {
				return nil, err
			}
			if p > 0 && p <= 65535 && !portMap[p] {
				ports = append(ports, p)
				portMap[p] = true
			}
		}
	}

	sort.Ints(ports)
	return ports, nil
}

func main() {
	myApp := app.New()
	myApp.Settings().SetTheme(theme.DefaultTheme())
	myWindow := myApp.NewWindow("局域网IP扫描器")
	myWindow.Resize(fyne.NewSize(900, 650))

	scanner := NewScanner()

	networkEntry := widget.NewEntry()
	networkEntry.SetText(scanner.getLocalIP())
	networkEntry.SetPlaceHolder("例如: 192.168.1.0/24")

	portsEntry := widget.NewEntry()
	portsEntry.SetText("21,22,23,25,53,80,110,135,139,143,443,445,993,995,1433,1521,3306,3389,5001,5432,5900,6379,8080,8443,27017")
	portsEntry.SetPlaceHolder("例如: 80,443,8080 或 1-1000")

	onlineCount := binding.NewInt()
	scanTime := binding.NewInt()
	portScanTime := binding.NewInt()
	onlineCount.Set(0)
	scanTime.Set(0)
	portScanTime.Set(0)

	countLabel := widget.NewLabelWithData(binding.IntToStringWithFormat(onlineCount, "在线设备: %d"))
	timeLabel := widget.NewLabelWithData(binding.IntToStringWithFormat(scanTime, "IP扫描耗时: %d秒"))
	portTimeLabel := widget.NewLabelWithData(binding.IntToStringWithFormat(portScanTime, "端口扫描耗时: %d秒"))

	progressBar := widget.NewProgressBar()
	portProgressBar := widget.NewProgressBar()
	statusLabel := widget.NewLabel("就绪")

	currentSelectedIP := binding.NewString()
	currentSelectedIP.Set("")

	ipList := widget.NewList(
		func() int {
			return len(scanner.getOnlineIPs())
		},
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			ips := scanner.getOnlineIPs()
			if id < len(ips) {
				label := obj.(*widget.Label)
				info := scanner.getIPInfo(ips[id])
				portCount := 0
				if info != nil {
					portCount = len(info.Ports)
				}
				label.SetText(fmt.Sprintf("%s (%d ports)", ips[id], portCount))
			}
		},
	)

	portTable := widget.NewTable(
		func() (int, int) {
			ip, _ := currentSelectedIP.Get()
			if ip == "" {
				return 1, 3
			}
			info := scanner.getIPInfo(ip)
			if info == nil || len(info.Ports) == 0 {
				return 1, 3
			}
			return len(info.Ports) + 1, 3
		},
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(id widget.TableCellID, obj fyne.CanvasObject) {
			label := obj.(*widget.Label)
			if id.Row == 0 {
				switch id.Col {
				case 0:
					label.SetText("端口")
					label.TextStyle = fyne.TextStyle{Bold: true}
				case 1:
					label.SetText("服务")
					label.TextStyle = fyne.TextStyle{Bold: true}
				case 2:
					label.SetText("状态")
					label.TextStyle = fyne.TextStyle{Bold: true}
				}
			} else {
				ip, _ := currentSelectedIP.Get()
				if ip == "" {
					label.SetText("")
					return
				}
				info := scanner.getIPInfo(ip)
				if info == nil || id.Row-1 >= len(info.Ports) {
					label.SetText("")
					return
				}
				portInfo := info.Ports[id.Row-1]
				switch id.Col {
				case 0:
					label.SetText(fmt.Sprintf("%d", portInfo.Port))
				case 1:
					label.SetText(portInfo.Service)
				case 2:
					label.SetText("开放")
				}
			}
		},
	)
	portTable.SetColumnWidth(0, 80)
	portTable.SetColumnWidth(1, 120)
	portTable.SetColumnWidth(2, 80)

	ipList.OnSelected = func(id widget.ListItemID) {
		ips := scanner.getOnlineIPs()
		if id < len(ips) {
			currentSelectedIP.Set(ips[id])
			portTable.Refresh()
		}
	}

	var scanButton *widget.Button
	var portScanButton *widget.Button
	var ticker *time.Ticker
	var portTicker *time.Ticker
	var startTime time.Time
	var portStartTime time.Time

	scanButton = widget.NewButton("扫描IP", func() {
		if scanner.isScanning() {
			scanner.stop()
			if ticker != nil {
				ticker.Stop()
			}
			fyne.Do(func() {
				scanButton.SetText("扫描IP")
				statusLabel.SetText("已停止")
			})
			return
		}

		network := networkEntry.Text
		if network == "" {
			return
		}

		resultChan := make(chan string, 254)
		progressChan := make(chan int, 254)
		doneChan := make(chan bool, 1)
		startTime = time.Now()

		scanner.mu.Lock()
		scanner.onlineIPs = make([]string, 0)
		scanner.ipInfoMap = make(map[string]*IPInfo)
		scanner.mu.Unlock()
		currentSelectedIP.Set("")
		fyne.Do(func() {
			ipList.Refresh()
			portTable.Refresh()
			onlineCount.Set(0)
			progressBar.SetValue(0)
			scanButton.SetText("停止扫描")
			statusLabel.SetText("IP扫描中...")
		})

		ticker = time.NewTicker(100 * time.Millisecond)
		go func() {
			for range ticker.C {
				elapsed := int(time.Since(startTime).Seconds())
				scanTime.Set(elapsed)
			}
		}()

		go scanner.scanNetwork(network, resultChan, progressChan, doneChan)

		completedCount := 0

		go func() {
			for {
				select {
				case ip, ok := <-resultChan:
					if !ok {
						return
					}
					scanner.addIP(ip)
					fyne.Do(func() {
						onlineCount.Set(len(scanner.getOnlineIPs()))
						ipList.Refresh()
					})
				case _, ok := <-progressChan:
					if !ok {
						return
					}
					completedCount++
					fyne.Do(func() {
						progressBar.SetValue(float64(completedCount) / 254.0)
					})
				case <-doneChan:
					if ticker != nil {
						ticker.Stop()
					}
					fyne.Do(func() {
						scanButton.SetText("扫描IP")
						statusLabel.SetText("IP扫描完成")
						progressBar.SetValue(1)
						ips := scanner.getOnlineIPs()
						if len(ips) > 0 {
							currentSelectedIP.Set(ips[0])
							ipList.Select(0)
						}
					})
					return
				}
			}
		}()
	})

	portScanButton = widget.NewButton("扫描端口", func() {
		if scanner.isScanning() {
			scanner.stop()
			if portTicker != nil {
				portTicker.Stop()
			}
			fyne.Do(func() {
				portScanButton.SetText("扫描端口")
				statusLabel.SetText("已停止")
			})
			return
		}

		ips := scanner.getOnlineIPs()
		if len(ips) == 0 {
			fyne.Do(func() {
				dialog.ShowInformation("提示", "请先扫描IP地址", myWindow)
			})
			return
		}

		ports, err := parsePorts(portsEntry.Text)
		if err != nil || len(ports) == 0 {
			fyne.Do(func() {
				dialog.ShowError(fmt.Errorf("端口格式错误，请输入正确的端口格式"), myWindow)
			})
			return
		}

		portResultChan := make(chan string, len(ips))
		portStartTime = time.Now()

		fyne.Do(func() {
			portScanButton.SetText("停止扫描")
			statusLabel.SetText("端口扫描中...")
		})

		portTicker = time.NewTicker(100 * time.Millisecond)
		go func() {
			for range portTicker.C {
				elapsed := int(time.Since(portStartTime).Seconds())
				portScanTime.Set(elapsed)
			}
		}()

		go scanner.scanAllPorts(ports, 800*time.Millisecond, portResultChan)

		go func() {
			for {
				ip, ok := <-portResultChan
				if !ok {
					goto finish
				}
				fyne.Do(func() {
					ipList.Refresh()
					selectedIP, _ := currentSelectedIP.Get()
					if selectedIP == ip {
						portTable.Refresh()
					}
				})
			}

		finish:
			if portTicker != nil {
				portTicker.Stop()
			}
			fyne.Do(func() {
				portScanButton.SetText("扫描端口")
				statusLabel.SetText("端口扫描完成")
				portProgressBar.SetValue(1)
				ipList.Refresh()
				portTable.Refresh()
			})
		}()
	})

	form := container.NewGridWithColumns(4,
		widget.NewLabel("网段:"),
		networkEntry,
		scanButton,
		widget.NewLabel(""),
	)

	portsForm := container.NewGridWithColumns(4,
		widget.NewLabel("端口:"),
		portsEntry,
		portScanButton,
		widget.NewLabel(""),
	)

	stats := container.NewGridWithColumns(3,
		countLabel,
		timeLabel,
		portTimeLabel,
	)

	progress := container.NewVBox(
		widget.NewLabel("IP扫描进度:"),
		progressBar,
		widget.NewLabel("端口扫描进度:"),
		portProgressBar,
		statusLabel,
	)

	leftPanel := container.NewBorder(
		widget.NewLabel("在线IP列表:"),
		nil, nil, nil,
		ipList,
	)

	rightPanel := container.NewBorder(
		widget.NewLabel("开放端口:"),
		nil, nil, nil,
		portTable,
	)

	splitContent := container.NewHSplit(leftPanel, rightPanel)
	splitContent.SetOffset(0.3)

	content := container.NewBorder(
		container.NewVBox(form, portsForm, stats, progress),
		nil, nil, nil,
		splitContent,
	)

	myWindow.SetContent(container.NewPadded(content))
	myWindow.ShowAndRun()
}

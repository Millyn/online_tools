package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/gen2brain/malgo"
)

// --- Windows API 常量与结构体 ---

const (
	SWP_NOSIZE     = 0x0001
	SWP_NOZORDER   = 0x0004
	SWP_SHOWWINDOW = 0x0040
	HWND_TOPMOST   = ^uintptr(0)
	HWND_NOTOPMOST = ^uintptr(1)
)

type RECT struct {
	Left, Top, Right, Bottom int32
}

// --- 极致性能音频缓冲区 ---

type RingBuffer struct {
	data []byte
	size int
	w    int
	r    int
	mu   sync.Mutex
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{data: make([]byte, size), size: size}
}

func (rb *RingBuffer) Write(b []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := len(b)
	if n > rb.size {
		b = b[n-rb.size:]
		n = rb.size
	}
	firstPart := rb.size - rb.w
	if n <= firstPart {
		copy(rb.data[rb.w:], b)
	} else {
		copy(rb.data[rb.w:], b[:firstPart])
		copy(rb.data[0:], b[firstPart:])
	}
	rb.w = (rb.w + n) % rb.size
}

func (rb *RingBuffer) Read(b []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := len(b)
	if n > rb.size {
		n = rb.size
	}
	firstPart := rb.size - rb.r
	if n <= firstPart {
		copy(b, rb.data[rb.r:rb.r+n])
	} else {
		copy(b[:firstPart], rb.data[rb.r:])
		copy(b[firstPart:], rb.data[:n-firstPart])
	}
	rb.r = (rb.r + n) % rb.size
}

// --- Windows 原生窗口操作函数 ---

var (
	user32            = syscall.NewLazyDLL("user32.dll")
	procFindWindowW   = user32.NewProc("FindWindowW")
	procSetWindowPos  = user32.NewProc("SetWindowPos")
	procGetWindowRect = user32.NewProc("GetWindowRect")
)

func findMyWindow(title string) uintptr {
	tPtr, _ := syscall.UTF16PtrFromString(title)
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(tPtr)))
	return hwnd
}

func getWindowPos(title string) (int32, int32) {
	hwnd := findMyWindow(title)
	if hwnd == 0 {
		return 0, 0
	}
	var rect RECT
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
	return rect.Left, rect.Top
}

func moveWindow(title string, x, y int32) {
	hwnd := findMyWindow(title)
	if hwnd != 0 {
		procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0, SWP_NOSIZE|SWP_NOZORDER|SWP_SHOWWINDOW)
	}
}

func optimizePriority() {
	if runtime.GOOS != "windows" {
		return
	}
	handle, _ := syscall.GetCurrentProcess()
	const BELOW_NORMAL_PRIORITY_CLASS = 0x00004000
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setPriority := kernel32.NewProc("SetPriorityClass")
	setPriority.Call(uintptr(handle), uintptr(BELOW_NORMAL_PRIORITY_CLASS))
}

// --- 数据结构 ---

type Config struct {
	IP               string             `json:"ip"`
	Port             string             `json:"port"`
	BtnCount         int                `json:"btn_count"`
	BtnNames         map[string]string  `json:"btn_names"`
	BtnCmds          map[string]string  `json:"btn_cmds"`
	LastListenDevice string             `json:"last_listen_device"`
	ActiveRelays     []string           `json:"active_relays"`
	AlwaysOnTop      bool               `json:"always_on_top"`
	WinX             int32              `json:"win_x"`
	WinY             int32              `json:"win_y"`
	BtnVolumes       map[string]float64 `json:"btn_volumes"`
}

const (
	configFileName = "obs_config_v3.json"
	windowTitle    = "OBS 控制专家 Pro"
)

// --- 音频引擎 ---

type RelayDevice struct {
	CapDev, PlayDev, Duplex *malgo.Device
	Ring                    *RingBuffer
	Volume                  *float64
}

func (rd *RelayDevice) Stop() {
	if rd.Duplex != nil {
		rd.Duplex.Uninit()
	}
	if rd.CapDev != nil {
		rd.CapDev.Uninit()
	}
	if rd.PlayDev != nil {
		rd.PlayDev.Uninit()
	}
}

type AudioEngine struct {
	ctx    *malgo.AllocatedContext
	relays map[string]*RelayDevice
	mu     sync.Mutex
}

func (e *AudioEngine) startRelay(capID, playID malgo.DeviceID, key string, isLoop, isFiiO bool, volPtr *float64) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if d, ok := e.relays[key]; ok {
		d.Stop()
		delete(e.relays, key)
	}

	relay := &RelayDevice{Ring: NewRingBuffer(44100 * 8), Volume: volPtr}

	if isFiiO || isLoop {
		capCfg := malgo.DefaultDeviceConfig(malgo.Capture)
		capCfg.Capture.DeviceID = capID.Pointer()
		capCfg.Capture.Format = malgo.FormatS32
		capCfg.Capture.Channels = 2
		capCfg.SampleRate = 44100
		if isLoop {
			capCfg.DeviceType = malgo.Loopback
		}

		onCap := func(pOut, pIn []byte, frameCount uint32) {
			relay.Ring.Write(pIn)
		}

		playCfg := malgo.DefaultDeviceConfig(malgo.Playback)
		playCfg.Playback.DeviceID = playID.Pointer()
		playCfg.Playback.Format = malgo.FormatS32
		playCfg.Playback.Channels = 2
		playCfg.SampleRate = 44100

		onPlay := func(pOut, pIn []byte, frameCount uint32) {
			relay.Ring.Read(pOut)
			samples := (*[1 << 28]int32)(unsafe.Pointer(&pOut[0]))[:frameCount*2]
			v := *relay.Volume
			if v != 1.0 {
				for i := range samples {
					samples[i] = int32(float64(samples[i]) * v)
				}
			}
		}

		var err error
		if relay.CapDev, err = malgo.InitDevice(e.ctx.Context, capCfg, malgo.DeviceCallbacks{Data: onCap}); err != nil {
			return err
		}
		if relay.PlayDev, err = malgo.InitDevice(e.ctx.Context, playCfg, malgo.DeviceCallbacks{Data: onPlay}); err != nil {
			return err
		}
		relay.CapDev.Start()
		relay.PlayDev.Start()
	} else {
		cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
		cfg.Capture.DeviceID = capID.Pointer()
		cfg.Playback.DeviceID = playID.Pointer()
		cfg.Capture.Format = malgo.FormatS16
		cfg.Playback.Format = malgo.FormatS16
		cfg.SampleRate = 44100
		var err error
		if relay.Duplex, err = malgo.InitDevice(e.ctx.Context, cfg, malgo.DeviceCallbacks{Data: func(pOut, pIn []byte, f uint32) {
			v := *relay.Volume
			if v == 1.0 {
				copy(pOut, pIn)
			} else {
				inS := (*[1 << 28]int16)(unsafe.Pointer(&pIn[0]))[:f*2]
				outS := (*[1 << 28]int16)(unsafe.Pointer(&pOut[0]))[:f*2]
				for i := range inS {
					val := float64(inS[i]) * v
					if val > 32767 {
						val = 32767
					} else if val < -32768 {
						val = -32768
					}
					outS[i] = int16(val)
				}
			}
		}}); err != nil {
			return err
		}
		relay.Duplex.Start()
	}
	e.relays[key] = relay
	return nil
}

// --- UI 构建 ---

func main() {
	optimizePriority()
	runtime.GOMAXPROCS(2)

	myApp := app.NewWithID("com.obs.remote.v3.pro")
	if iconResource, err := fyne.LoadResourceFromPath("app_icon.png"); err == nil {
		myApp.SetIcon(iconResource)
	}

	myWindow := myApp.NewWindow(windowTitle)
	conf := loadConfig()
	mCtx, _ := malgo.InitContext([]malgo.Backend{malgo.BackendWasapi}, malgo.ContextConfig{}, nil)
	engine := &AudioEngine{ctx: mCtx, relays: make(map[string]*RelayDevice)}

	myWindow.SetOnClosed(func() {
		x, y := getWindowPos(windowTitle)
		if x >= 0 && y >= 0 {
			conf.WinX = x
			conf.WinY = y
		}
		saveConfig(conf)
	})

	setStatus := func(lbl *widget.Label, status string) {
		fyne.Do(func() {
			switch status {
			case "loading":
				lbl.SetText("[尝试中...]")
			case "success":
				lbl.SetText("[监听中]")
			case "error":
				lbl.SetText("[错误]")
			default:
				lbl.SetText("[闲置]")
			}
		})
	}

	sendCmd := func(cmd string) {
		go func() {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(conf.IP, conf.Port), 500*time.Millisecond)
			if err == nil {
				conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
				conn.Write([]byte(cmd + "\n"))
				conn.Close()
			}
		}()
	}

	buttonList := container.NewVBox()
	refreshButtons := func() {
		buttonList.Objects = nil
		for i := 1; i <= conf.BtnCount; i++ {
			id := strconv.Itoa(i)
			name, cmd := conf.BtnNames[id], conf.BtnCmds[id]
			if name == "" {
				name = "按钮 " + id
			}
			if cmd == "" {
				cmd = id
			}
			buttonList.Add(widget.NewButtonWithIcon(name, theme.MediaPlayIcon(), func() { sendCmd(cmd) }))
		}
	}
	refreshButtons()

	pDevs, _ := mCtx.Context.Devices(malgo.Playback)
	cDevs, _ := mCtx.Context.Devices(malgo.Capture)
	var pNames []string
	for _, d := range pDevs {
		pNames = append(pNames, d.Name())
	}
	pSel := widget.NewSelect(pNames, func(s string) { conf.LastListenDevice = s; saveConfig(conf) })
	pSel.SetSelected(conf.LastListenDevice)

	audioList := container.NewVBox()
	deviceIDs := make(map[string]malgo.DeviceID)

	handleRelay := func(name string, check *widget.Check, statusLbl *widget.Label, volContainer *fyne.Container, vPtr *float64, volSlider *widget.Slider) {
		if check.Checked {
			volContainer.Show()
			setStatus(statusLbl, "loading")
			var pID malgo.DeviceID
			found := false
			for _, pd := range pDevs {
				if pd.Name() == pSel.Selected {
					pID = pd.ID
					found = true
					break
				}
			}
			if !found {
				setStatus(statusLbl, "error")
				check.SetChecked(false)
				return
			}

			if err := engine.startRelay(deviceIDs[name], pID, name, strings.HasPrefix(name, "[系统]"), strings.Contains(name, "FiiO"), vPtr); err != nil {
				setStatus(statusLbl, "error")
				check.SetChecked(false)
			} else {
				setStatus(statusLbl, "success")
				conf.ActiveRelays = append(conf.ActiveRelays, name)
			}
		} else {
			volContainer.Hide()
			engine.mu.Lock()
			if d, ok := engine.relays[name]; ok {
				d.Stop()
				delete(engine.relays, name)
			}
			engine.mu.Unlock()
			setStatus(statusLbl, "idle")
			newActive := []string{}
			for _, v := range conf.ActiveRelays {
				if v != name {
					newActive = append(newActive, v)
				}
			}
			conf.ActiveRelays = newActive
		}
		saveConfig(conf)
	}

	loadDevUI := func(devs []malgo.DeviceInfo, prefix string) {
		for _, d := range devs {
			name := prefix + d.Name()
			deviceIDs[name] = d.ID

			lbl := widget.NewLabel("[闲置]")
			lbl.TextStyle = fyne.TextStyle{Monospace: true}

			// 1. 准备音量数据指针和显示文本
			curVol, ok := conf.BtnVolumes[name]
			if !ok {
				curVol = 1.0
				conf.BtnVolumes[name] = 1.0
			}
			vPtr := new(float64)
			*vPtr = curVol

			volValLabel := widget.NewLabel(fmt.Sprintf("%d%%", int(curVol*100)))
			volValLabel.Alignment = fyne.TextAlignTrailing

			// 2. 创建滑块并绑定事件
			slider := widget.NewSlider(0, 150)
			slider.Step = 1.0
			slider.SetValue(curVol * 100)
			slider.OnChanged = func(f float64) {
				conf.BtnVolumes[name] = f / 100.0
				*vPtr = conf.BtnVolumes[name]
				volValLabel.SetText(fmt.Sprintf("%d%%", int(f)))
				saveConfig(conf)
			}

			// 3. 将滑块和百分比文字组合 (Border 布局解决溢出和显示)
			// Border: Left 为滑块，Right 为百分比文字
			volBar := container.NewBorder(nil, nil, nil, volValLabel, slider)
			volContainer := container.NewPadded(volBar) // Padded 解决 UI BUG，防止滑块顶到边缘
			volContainer.Hide()

			ck := widget.NewCheck(name, nil)
			ck.OnChanged = func(b bool) { handleRelay(name, ck, lbl, volContainer, vPtr, slider) }

			// 初始化恢复状态
			for _, active := range conf.ActiveRelays {
				if active == name {
					ck.Checked = true
					go func(n string, c *widget.Check, l *widget.Label, vc *fyne.Container, vp *float64, s *widget.Slider) {
						time.Sleep(500 * time.Millisecond)
						handleRelay(n, c, l, vc, vp, s)
					}(name, ck, lbl, volContainer, vPtr, slider)
				}
			}

			row := container.NewVBox(
				container.NewBorder(nil, nil, ck, nil, lbl),
				volContainer,
			)
			audioList.Add(row)
		}
	}
	loadDevUI(cDevs, "")
	loadDevUI(pDevs, "[系统] ")

	ipEntry, portEntry := widget.NewEntry(), widget.NewEntry()
	ipEntry.SetText(conf.IP)
	portEntry.SetText(conf.Port)
	countEntry := widget.NewEntry()
	countEntry.SetText(strconv.Itoa(conf.BtnCount))

	editBox := container.NewVBox()
	refreshEditUI := func() {
		editBox.Objects = nil
		for i := 1; i <= conf.BtnCount; i++ {
			id := strconv.Itoa(i)
			nE, cE := widget.NewEntry(), widget.NewEntry()
			nE.SetText(conf.BtnNames[id])
			cE.SetText(conf.BtnCmds[id])
			nE.OnChanged = func(s string) { conf.BtnNames[id] = s; saveConfig(conf); refreshButtons() }
			cE.OnChanged = func(s string) { conf.BtnCmds[id] = s; saveConfig(conf); refreshButtons() }
			editBox.Add(container.NewGridWithColumns(2, nE, cE))
		}
	}
	refreshEditUI()

	// 移除强制宽度的 GridWrap，使用普通的 Scroll 配合 VBox
	scrollableEdit := container.NewVScroll(editBox)
	scrollableEdit.SetMinSize(fyne.NewSize(320, 200))

	scrollableAudio := container.NewVScroll(audioList)
	scrollableAudio.SetMinSize(fyne.NewSize(320, 240))

	accordion := widget.NewAccordion(
		widget.NewAccordionItem("网络与按钮", container.NewVBox(
			container.NewGridWithColumns(2, ipEntry, portEntry),
			container.NewBorder(nil, nil, nil, widget.NewButton("更新", func() {
				if c, _ := strconv.Atoi(countEntry.Text); c > 0 {
					conf.BtnCount = c
					saveConfig(conf)
					refreshButtons()
					refreshEditUI()
				}
			}), countEntry),
			scrollableEdit,
		)),
		widget.NewAccordionItem("音频监听", container.NewVBox(pSel, scrollableAudio)),
	)

	mainTabs := container.NewAppTabs(
		container.NewTabItemWithIcon("", theme.HomeIcon(), container.NewBorder(
			widget.NewCheck("置顶", func(b bool) { conf.AlwaysOnTop = b; saveConfig(conf); setAlwaysOnTop(windowTitle, b) }),
			nil, nil, nil, container.NewVScroll(buttonList),
		)),
		container.NewTabItemWithIcon("", theme.SettingsIcon(), accordion),
	)

	myWindow.SetContent(mainTabs)
	myWindow.Resize(fyne.NewSize(380, 480)) // 稍微增加一点默认宽度防止 UI 拥挤

	go func() {
		time.Sleep(300 * time.Millisecond)
		if conf.WinX != 0 || conf.WinY != 0 {
			moveWindow(windowTitle, conf.WinX, conf.WinY)
		}
		if conf.AlwaysOnTop {
			setAlwaysOnTop(windowTitle, true)
		}
	}()

	myWindow.ShowAndRun()
}

func setAlwaysOnTop(title string, topmost bool) {
	if runtime.GOOS != "windows" {
		return
	}
	if hwnd := findMyWindow(title); hwnd != 0 {
		z := HWND_NOTOPMOST
		if topmost {
			z = HWND_TOPMOST
		}
		procSetWindowPos.Call(hwnd, z, 0, 0, 0, 0, SWP_NOSIZE|0x0002)
	}
}

func loadConfig() Config {
	c := Config{IP: "127.0.0.1", Port: "5631", BtnCount: 4, BtnVolumes: make(map[string]float64)}
	if b, err := os.ReadFile(configFileName); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.BtnNames == nil {
		c.BtnNames = make(map[string]string)
	}
	if c.BtnCmds == nil {
		c.BtnCmds = make(map[string]string)
	}
	if c.BtnVolumes == nil {
		c.BtnVolumes = make(map[string]float64)
	}
	return c
}

func saveConfig(c Config) {
	b, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(configFileName, b, 0644)
}

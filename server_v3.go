package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw" // 标准库
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andreykaipov/goobs"
	"github.com/andreykaipov/goobs/api/requests/inputs"
	"github.com/andreykaipov/goobs/api/requests/sources"
	xdraw "golang.org/x/image/draw" // 扩展库别名
)

// --- 配置区 ---
const (
	obsAddr     = "127.0.0.1:4455"
	obsPassword = "BgeF9IZIv13ayslb"
	sourceName  = "采集屏幕"
	debugDir    = "./debug_images" // 调试图片保存路径
)

// --- 辅助指针工具 ---
func ptrString(s string) *string    { return &s }
func ptrFloat64(f float64) *float64 { return &f }

// --- 全局变量 ---
var (
	user32            = syscall.NewLazyDLL("user32.dll")
	procKeybdEvent    = user32.NewProc("keybd_event")
	procMapVirtualKey = user32.NewProc("MapVirtualKeyW")

	lastFingers   = make(map[string]string)
	fingerMutex   sync.Mutex
	currentStatus = "CAPTURE"
)

const (
	KEYEVENTF_KEYUP    = 0x0002
	KEYEVENTF_SCANCODE = 0x0008
)

var keyMap = map[string]uintptr{
	"ctrl": 0x11, "shift": 0x10, "alt": 0x12,
	"1": 0x31, "2": 0x32, "3": 0x33, "4": 0x34, "5": 0x35,
	"6": 0x36, "7": 0x37, "8": 0x38, "9": 0x39, "0": 0x30,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74,
}

// --- 逻辑函数 ---

// 保存调试图片到本地
func saveDebugImage(data []byte, finger string) {
	// 如果文件夹不存在则创建
	if _, err := os.Stat(debugDir); os.IsNotExist(err) {
		_ = os.MkdirAll(debugDir, 0755)
	}

	// 文件名格式：时分秒_指纹前8位.jpg
	fileName := fmt.Sprintf("%s_%s.jpg", time.Now().Format("150405"), finger[:8])
	filePath := filepath.Join(debugDir, fileName)

	err := os.WriteFile(filePath, data, 0644)
	if err != nil {
		log.Printf("[Debug] 写入图片文件失败: %v", err)
	} else {
		log.Printf("[Debug] 图片已保存便于预览: %s", filePath)
	}
}

// 生成图片指纹 (调整为 128x128 以获取更高的对比精度)
func getFingerprint(img image.Image) string {
	// 1. 定义 256x256 的采样规格
	const size = 256
	smallImg := image.NewRGBA(image.Rect(0, 0, size, size))

	// 2. 缩放图片 (使用 xdraw 保持高质量)
	xdraw.ApproxBiLinear.Scale(smallImg, smallImg.Bounds(), img, img.Bounds(), draw.Over, nil)

	// 3. 计算灰度指纹
	// 256x256 总计 65,536 个采样点
	data := make([]byte, size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			r, g, b, _ := smallImg.At(x, y).RGBA()
			// 灰度公式：Y = 0.299R + 0.587G + 0.114B
			// 由于 RGBA() 返回的是 0-65535，所以除以 256 得到 0-255 的 uint8
			gray := uint8((r*299 + g*587 + b*114) / (1000 * 256))
			data[y*size+x] = gray
		}
	}

	// 4. 返回 MD5 指纹
	return fmt.Sprintf("%x", md5.Sum(data))
}

// 健康检查
func checkHealth() {
	client, err := goobs.New(obsAddr, goobs.WithPassword(obsPassword))
	if err != nil {
		log.Printf("[Health] OBS 连接失败: %v", err)
		return
	}
	defer client.Disconnect()

	// 1. 获取截图
	resp, err := client.Sources.GetSourceScreenshot(&sources.GetSourceScreenshotParams{
		SourceName:              ptrString(sourceName),
		ImageFormat:             ptrString("jpg"),
		ImageWidth:              ptrFloat64(640.0), // 提升原始截图宽度，为 256 采样提供足够像素
		ImageHeight:             ptrFloat64(360.0), // 提升原始截图高度
		ImageCompressionQuality: ptrFloat64(50.0),  // 适度提升质量，减少 JPEG 压缩伪影
	})
	if err != nil {
		log.Printf("[Health] 获取截图失败: %v", err)
		return
	}

	// 2. 处理 Base64
	imageData := resp.ImageData
	if i := strings.Index(imageData, ","); i != -1 {
		imageData = imageData[i+1:]
	}
	unbased, err := base64.StdEncoding.DecodeString(imageData)
	if err != nil {
		log.Printf("[Health] Base64解码错误: %v", err)
		return
	}

	// 3. 解码图片获取格式
	img, format, err := image.Decode(bytes.NewReader(unbased))
	if err != nil {
		log.Printf("[Health] 图片解码失败: %v", err)
		return
	}

	// 4. 计算指纹
	currentFinger := getFingerprint(img)

	// --- 核心调试功能：保存到本地 ---
	//saveDebugImage(unbased, currentFinger)

	fingerMutex.Lock()
	lastFinger, exists := lastFingers[sourceName]
	lastFingers[sourceName] = currentFinger
	fingerMutex.Unlock()

	if !exists {
		fmt.Printf("[Health] 首次记录指纹: %s，图片已存至 %s\n", currentFinger, debugDir)
		return
	}

	if currentFinger == lastFinger {
		fmt.Printf("[ALARM] 画面特征未变(指纹: %s)，正在尝试重置源...\n", currentFinger)
		refreshOBSSource()
	} else {
		fmt.Printf("[Health] 运行正常 (指纹: %s, 格式: %s)\n", currentFinger, format)
	}
}

// 刷新 OBS 采集源驱动
func refreshOBSSource() {
	client, err := goobs.New(obsAddr, goobs.WithPassword(obsPassword))
	if err != nil {
		return
	}
	defer client.Disconnect()

	target := sourceName
	resp, err := client.Inputs.GetInputSettings(&inputs.GetInputSettingsParams{InputName: &target})
	if err != nil {
		log.Printf("[Action] 获取源设置失败: %v", err)
		return
	}

	settings := resp.InputSettings
	deviceID := settings["video_device_id"]

	// 物理断开逻辑
	offSettings := map[string]interface{}{
		"video_device_id": "",
		"active":          false,
	}
	_, _ = client.Inputs.SetInputSettings(&inputs.SetInputSettingsParams{
		InputName:     &target,
		InputSettings: offSettings,
	})

	time.Sleep(2 * time.Second)

	// 物理重连逻辑
	settings["video_device_id"] = deviceID
	settings["active"] = true
	_, err = client.Inputs.SetInputSettings(&inputs.SetInputSettingsParams{
		InputName:     &target,
		InputSettings: settings,
	})

	if err == nil {
		fmt.Println("[Action] 采集卡驱动重载完成")
	}
}

// 键盘模拟逻辑
func simulateKeys(line string) {
	if line == "5" {
		currentStatus = "BRB"
	} else {
		currentStatus = "CAPTURE"
	}
	keys := strings.Split(line, ",")
	var vks []uintptr
	for _, k := range keys {
		if vk, ok := keyMap[strings.ToLower(strings.TrimSpace(k))]; ok {
			vks = append(vks, vk)
		}
	}
	for _, vk := range vks {
		scanCode, _, _ := procMapVirtualKey.Call(vk, 0)
		procKeybdEvent.Call(vk, scanCode, KEYEVENTF_SCANCODE, 0)
	}
	time.Sleep(20 * time.Millisecond)
	for i := len(vks) - 1; i >= 0; i-- {
		vk := vks[i]
		scanCode, _, _ := procMapVirtualKey.Call(vk, 0)
		procKeybdEvent.Call(vk, scanCode, KEYEVENTF_SCANCODE|KEYEVENTF_KEYUP, 0)
	}
	checkHealth()
}

func main() {
	ln, err := net.Listen("tcp", ":5631")
	if err != nil {
		log.Fatal("端口监听失败: ", err)
	}
	fmt.Println("=== 全能管理服务端 v4 (含截图调试) ===")
	fmt.Printf("1. 监听端口: 5631\n")

	// --- 新增：定时器任务 ---
	go func() {
		// 启动后先延迟几秒执行第一次检查，确保 OBS 已完全就绪
		time.Sleep(5 * time.Second)
		log.Println("[Scheduler] 开启自动健康检查任务...")

		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		// 立即执行第一次
		checkHealth()

		for range ticker.C {
			if currentStatus == "CAPTURE" {
				checkHealth()
			} else {
				log.Printf("当前是 %v 模式 不检查", currentStatus)
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			scanner := bufio.NewScanner(c)
			for scanner.Scan() {
				cmd := strings.TrimSpace(scanner.Text())
				switch cmd {
				case "REFRESH_SOURCE":
					refreshOBSSource()
				case "CHECK_HEALTH":
					checkHealth()
				case "":
					continue
				default:
					simulateKeys(cmd)
				}
			}
		}(conn)
	}
}

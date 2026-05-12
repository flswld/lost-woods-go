package game

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"sort"
	"strconv"
	"time"

	"hk4e/common/constant"
	"hk4e/gs/model"
	"hk4e/protocol/proto"

	"github.com/flswld/halo/logger"
	"gitlab.com/gomidi/midi/v2"
	"gitlab.com/gomidi/midi/v2/smf"
)

// 音视频 模块 - 项目中两个有趣的玩具功能
//
// 1) MIDI 弹奏（PlayAudio + StartMidiInputDev）：
//   - 把 .mid 文件解析成音符流 → 通过 SceneAudioNotify 让原神客户端播放对应音符
//   - 原神场景内的"风物之诗琴"等乐器可发音 服务端控制弹奏什么
//   - 支持 PC 上接 MIDI 键盘实时弹奏（需要 librtmidi 见 audio_video_rtmidi.go）
//   - AudioChan 是音符 channel 主循环 onUserTick 取出后发 SceneAudioNotify
//
// 2) JPEG 像素屏（LoadFrameFile + UpdateFrame）：
//   - 80×80 像素的彩色图片用 7 种颜色 gadget 在 AI 世界场景 3 摆出来
//   - 默认坐标（2700, 200, -1800）是预留的空地
//   - rgb=true 全彩；rgb=false 仅黑白二值化
//   - 灰度值映射到 7 色 gadget（CalcColorLight 算每个颜色的亮度排序）
//
// 这两个功能与游戏玩法无关 是作者的"创意演示"性质的实验品

const (
	KeyOffset              = -12 * 1 // 八度修正偏移（MIDI key → 原神音符 偏移 12 半音 = 1 个八度）
	MidiInputDevPortNumber = 0       // 默认 MIDI 输入设备端口号（外接 MIDI 键盘）
)

var (
	AudioChan               = make(chan uint32, 1000)
	MidiInputDevStop func() = nil
)

func sendMidiMsg(msg midi.Message) {
	var channel, key, velocity uint8
	if !msg.GetNoteStart(&channel, &key, &velocity) {
		return
	}
	// 60 -> 中央C C4
	note := int32(key) + int32(KeyOffset)
	if note < 36 || note > 96 {
		return
	}
	AudioChan <- uint32(note)
}

// PlayAudio 播放 MIDI 文件（GMCmd.AvPlayAudio 入口）
//
// 处理：
//  1. 解析 SMF 格式 取 BPM 计算每 tick 对应的毫秒数
//  2. 每个 track 起一个 goroutine 按 Delta 时间间隔播放
//  3. 每个音符通过 sendMidiMsg → AudioChan → 主循环发 SceneAudioNotify
//
// 仅支持单一 BPM 的 MIDI 文件（多次 Tempo 变化的复杂曲谱不支持）
// 上传方式：把 .mid 转 base64 通过 AvPlayAudio 命令发给服务器
func PlayAudio(fileData []byte) {
	reader := bytes.NewReader(fileData)
	audio, err := smf.ReadFrom(reader)
	if err != nil {
		logger.Error("read midi file error: %v", err)
		return
	}
	tempoChangeList := audio.TempoChanges()
	if len(tempoChangeList) != 1 {
		logger.Error("midi file format not support")
		return
	}
	tempoChange := tempoChangeList[0]
	metricTicks := audio.TimeFormat.(smf.MetricTicks)
	tickTime := ((60000000.0 / tempoChange.BPM) / float64(metricTicks.Resolution())) / 1000.0
	logger.Debug("start play audio")
	// 全部轨道
	for _, track := range audio.Tracks {
		// 单个轨道
		go func(track smf.Track) {
			for _, event := range track {
				delay := uint32(float64(event.Delta) * tickTime)
				// busyPollWaitMilliSecond(delay)
				interruptWaitMilliSecond(delay)
				sendMidiMsg(midi.Message(event.Message))
			}
		}(track)
	}
}

func interruptWaitMilliSecond(delay uint32) {
	time.Sleep(time.Millisecond * time.Duration(delay))
}

func busyPollWaitMilliSecond(delay uint32) {
	start := time.Now()
	end := start.Add(time.Millisecond * time.Duration(delay))
	for {
		now := time.Now()
		if now.After(end) {
			break
		}
	}
}

// StartMidiInputDev 启动 MIDI 输入设备监听（GMCmd.AvStartMidiInputDev 入口）
// 把外接 MIDI 键盘的实时弹奏转发到游戏内 让玩家用真实键盘弹原神乐器
// 使用 gomidi/midi 库 底层 Windows 走 librtmidi.dll（见 audio_video_rtmidi.go）
func StartMidiInputDev() error {
	logger.Info("midi input dev port: %v", midi.GetInPorts())
	in, err := midi.InPort(MidiInputDevPortNumber)
	if err != nil {
		return err
	}
	MidiInputDevStop, err = midi.ListenTo(in, func(msg midi.Message, timestampms int32) {
		logger.Debug("midi input dev msg: %v", msg)
		sendMidiMsg(msg)
	})
	if err != nil {
		return err
	}
	return nil
}

func StopMidiInputDev() {
	MidiInputDevStop()
	midi.CloseDriver()
}

// JPEG 像素屏 - 用 7 色 gadget 在游戏世界里"显示"图片

const (
	SCREEN_WIDTH  = 80  // 像素宽（80 像素）
	SCREEN_HEIGHT = 80  // 像素高（80 像素）
	SCREEN_DPI    = 0.5 // 每像素物理距离（米）→ 80*0.5=40 米的屏幕
)

const GADGET_ID = 70590015 // 黑白模式默认用的 gadget id（黄色"光"）

var SCREEN_ENTITY_ID_LIST []uint32
var FRAME_COLOR [][]int
var FRAME [][]bool

const (
	GADGET_RED       = 70590016
	GADGET_GREEN     = 70590019
	GADGET_BLUE      = 70590017
	GADGET_CYAN      = 70590014
	GADGET_YELLOW    = 70590015
	GADGET_CYAN_BLUE = 70590018
	GADGET_PURPLE    = 70590020
)

const (
	RED_RGB       = "C3764F"
	GREEN_RGB     = "559F30"
	BLUE_RGB      = "6293EA"
	CYAN_RGB      = "479094"
	YELLOW_RGB    = "DBB643"
	CYAN_BLUE_RGB = "2B89C9"
	PURPLE_RGB    = "6E5BC5"
)

var COLOR_GADGET_MAP = map[string]int{
	RED_RGB:       GADGET_RED,
	GREEN_RGB:     GADGET_GREEN,
	BLUE_RGB:      GADGET_BLUE,
	CYAN_RGB:      GADGET_CYAN,
	YELLOW_RGB:    GADGET_YELLOW,
	CYAN_BLUE_RGB: GADGET_CYAN_BLUE,
	PURPLE_RGB:    GADGET_PURPLE,
}

var ALL_COLOR = []string{RED_RGB, GREEN_RGB, BLUE_RGB, CYAN_RGB, YELLOW_RGB, CYAN_BLUE_RGB, PURPLE_RGB}

type ColorLight struct {
	Color string
	Light uint8
}

type COLOR_LIGHT_LIST_SORT []*ColorLight

var COLOR_LIGHT_LIST COLOR_LIGHT_LIST_SORT

func (s COLOR_LIGHT_LIST_SORT) Len() int {
	return len(s)
}

func (s COLOR_LIGHT_LIST_SORT) Less(i, j int) bool {
	return s[i].Light < s[j].Light
}

func (s COLOR_LIGHT_LIST_SORT) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func init() {
	CalcColorLight()
}

// CalcColorLight 计算 7 种颜色的灰度亮度并按亮度排序
// 灰度公式：gray = R*0.299 + G*0.587 + B*0.114（YUV 转换中的亮度系数）
// 排序后按"亮度等差"重新分配 用于灰度→颜色的映射查表
func CalcColorLight() {
	COLOR_LIGHT_LIST = make(COLOR_LIGHT_LIST_SORT, 0)
	for _, c := range ALL_COLOR {
		r, g, b := GetColorRGB(c)
		gray := float32(r)*0.299 + float32(g)*0.587 + float32(b)*0.114
		COLOR_LIGHT_LIST = append(COLOR_LIGHT_LIST, &ColorLight{
			Color: c,
			Light: uint8(gray),
		})
	}
	sort.Stable(COLOR_LIGHT_LIST)
	total := len(COLOR_LIGHT_LIST)
	div := 255.0 / float32(total)
	for index, colorLight := range COLOR_LIGHT_LIST {
		colorLight.Light = uint8(div * float32(index+1))
	}
}

func GetColorRGB(c string) (r, g, b uint8) {
	if len(c) != 6 {
		return 0, 0, 0
	}
	rr, err := strconv.ParseUint(c[0:2], 16, 8)
	if err != nil {
		return 0, 0, 0
	}
	r = uint8(rr)
	gg, err := strconv.ParseUint(c[2:4], 16, 8)
	if err != nil {
		return 0, 0, 0
	}
	g = uint8(gg)
	bb, err := strconv.ParseUint(c[4:6], 16, 8)
	if err != nil {
		return 0, 0, 0
	}
	b = uint8(bb)
	return r, g, b
}

func ReadJpgFile(fileName string) image.Image {
	file, err := os.Open(fileName)
	if err != nil {
		return nil
	}
	defer func() {
		_ = file.Close()
	}()
	img, err := jpeg.Decode(file)
	if err != nil {
		return nil
	}
	return img
}

func WriteJpgFile(fileName string, jpg image.Image) {
	file, err := os.Create(fileName)
	if err != nil {
		return
	}
	defer func() {
		_ = file.Close()
	}()
	err = jpeg.Encode(file, jpg, &jpeg.Options{
		Quality: 100,
	})
	if err != nil {
		return
	}
}

// LoadFrameFile 加载并预处理 JPEG 图片
//
// 处理：
//  1. 解码 JPEG 到内存
//  2. 灰度化每个像素（YUV 公式）
//  3. 计算全图平均灰度 grayAvg → 用于黑白二值化阈值
//  4. 按灰度查找最近的 7 色之一 填到 FRAME_COLOR[w][h]（rgb 模式用）
//  5. 灰度高于 grayAvg 的填到 FRAME[w][h] = true（黑白模式用）
func LoadFrameFile(fileData []byte) error {
	reader := bytes.NewReader(fileData)
	frameImg, err := jpeg.Decode(reader)
	if err != nil {
		return err
	}
	FRAME = make([][]bool, SCREEN_WIDTH)
	for w := 0; w < SCREEN_WIDTH; w++ {
		FRAME[w] = make([]bool, SCREEN_HEIGHT)
	}
	FRAME_COLOR = make([][]int, SCREEN_WIDTH)
	for w := 0; w < SCREEN_WIDTH; w++ {
		FRAME_COLOR[w] = make([]int, SCREEN_HEIGHT)
	}
	grayAvg := uint64(0)
	grayImg := image.NewRGBA(image.Rect(0, 0, SCREEN_WIDTH, SCREEN_HEIGHT))
	for w := 0; w < SCREEN_WIDTH; w++ {
		for h := 0; h < SCREEN_HEIGHT; h++ {
			pix := frameImg.At(w, h)
			r, g, b, _ := pix.RGBA()
			gray := float32(r>>8)*0.299 + float32(g>>8)*0.587 + float32(b>>8)*0.114
			grayImg.SetRGBA(w, h, color.RGBA{R: uint8(gray), G: uint8(gray), B: uint8(gray), A: 255})
			grayAvg += uint64(gray)
		}
	}
	grayAvg /= SCREEN_WIDTH * SCREEN_HEIGHT
	rgbImg := image.NewRGBA(image.Rect(0, 0, SCREEN_WIDTH, SCREEN_HEIGHT))
	binImg := image.NewRGBA(image.Rect(0, 0, SCREEN_WIDTH, SCREEN_HEIGHT))
	for w := 0; w < SCREEN_WIDTH; w++ {
		for h := 0; h < SCREEN_HEIGHT; h++ {
			pix := frameImg.At(w, h)
			r, g, b, _ := pix.RGBA()
			gray := float32(r>>8)*0.299 + float32(g>>8)*0.587 + float32(b>>8)*0.114
			c := ""
			for _, colorLight := range COLOR_LIGHT_LIST {
				if float32(colorLight.Light) > gray {
					c = colorLight.Color
					break
				}
			}
			if c == "" {
				c = COLOR_LIGHT_LIST[len(COLOR_LIGHT_LIST)-1].Color
			}
			rr, gg, bb := GetColorRGB(c)
			rgbImg.SetRGBA(w, h, color.RGBA{R: rr, G: gg, B: bb, A: 255})
			FRAME_COLOR[w][h] = COLOR_GADGET_MAP[c]
			if gray > float32(grayAvg) {
				FRAME[w][h] = true
				binImg.SetRGBA(w, h, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}
	return nil
}

// UpdateFrame 更新像素屏画面（GMCmd.AvUpdateFrame 入口）
//
// 处理：
//  1. LoadFrameFile 处理图片
//  2. 销毁旧的像素 gadget 实体（一次最多 6400 个）
//  3. 创建 80×80=6400 个 gadget 实体 用 SCREEN_DPI=0.5m 间距摆出来
//  4. AddSceneEntityNotify 广播给场景内玩家
//
// rgb=true 全彩（每个像素用对应颜色 gadget）
// rgb=false 黑白（仅亮像素位置摆默认颜色 gadget 暗像素留空）
//
// 性能警告：6400 个实体一次性创建/销毁 不适合频繁切图
//
//	作者拿来当玩具不是真正的"动画屏幕"
func UpdateFrame(fileData []byte, basePos *model.Vector, rgb bool) {
	err := LoadFrameFile(fileData)
	if err != nil {
		return
	}
	world := WORLD_MANAGER.GetAiWorld()
	scene := world.GetSceneById(3)
	for _, v := range SCREEN_ENTITY_ID_LIST {
		scene.DestroyEntity(v)
	}
	GAME.RemoveSceneEntityNotifyBroadcast(scene, proto.VisionType_VISION_REMOVE, SCREEN_ENTITY_ID_LIST, 0)
	SCREEN_ENTITY_ID_LIST = make([]uint32, 0)
	leftTopPos := &model.Vector{
		X: basePos.X + float64(SCREEN_WIDTH)*SCREEN_DPI/2,
		Y: basePos.Y + float64(SCREEN_HEIGHT)*SCREEN_DPI,
		Z: basePos.Z,
	}
	for w := 0; w < SCREEN_WIDTH; w++ {
		for h := 0; h < SCREEN_HEIGHT; h++ {
			// 创建像素点
			if rgb {
				gadgetNormalEntity := scene.CreateEntityGadgetNormal(&model.Vector{
					X: leftTopPos.X - float64(w)*SCREEN_DPI,
					Y: leftTopPos.Y - float64(h)*SCREEN_DPI,
					Z: leftTopPos.Z,
				}, new(model.Vector), 0, 0, constant.VISION_LEVEL_SUPER, uint32(FRAME_COLOR[w][h]), uint32(constant.GADGET_STATE_DEFAULT))
				scene.CreateEntity(gadgetNormalEntity)
				SCREEN_ENTITY_ID_LIST = append(SCREEN_ENTITY_ID_LIST, gadgetNormalEntity.GetId())
			} else {
				if !FRAME[w][h] {
					gadgetNormalEntity := scene.CreateEntityGadgetNormal(&model.Vector{
						X: leftTopPos.X - float64(w)*SCREEN_DPI,
						Y: leftTopPos.Y - float64(h)*SCREEN_DPI,
						Z: leftTopPos.Z,
					}, new(model.Vector), 0, 0, constant.VISION_LEVEL_SUPER, uint32(GADGET_ID), uint32(constant.GADGET_STATE_DEFAULT))
					scene.CreateEntity(gadgetNormalEntity)
					SCREEN_ENTITY_ID_LIST = append(SCREEN_ENTITY_ID_LIST, gadgetNormalEntity.GetId())
				}
			}
		}
	}
	GAME.AddSceneEntityNotify(world.GetOwner(), proto.VisionType_VISION_BORN, SCREEN_ENTITY_ID_LIST, true, false)
}

package common

import (
	"crypto/sha256"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/lxn/win"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	"io"
	"m4s-converter/conver"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

//go:embed ffmpeg.exe
var ffmpegFile embed.FS

var (
	FFmpegName    = "ffmpeg.exe"
	FileHashValue = "3b805cb66ebb0e68f19c939bece693c345b15b7bf277b572ab7b4792ee65aad8"
)

type Config struct {
	FFMpegPath string
	CachePath  string
	Overlay    string
	File       *os.File
	AssPath    string
	AssOFF     bool
}

func (c *Config) InitConfig() {
	InitLog()
	overlay := flag.Bool("o", false, "是否覆盖已存在的视频，默认不覆盖") //nolint
	c.AssOFF = *flag.Bool("a", false, "是否关闭自动生成ass弹幕，默认不关闭")
	c.FFMpegPath = *flag.String("f", "", "指定FFMpeg路径，默认使用自带的FFMpeg文件")
	c.CachePath = *flag.String("c", "", "指定缓存路径，默认使用bilibili默认缓存路径")
	version := flag.Bool("v", false, "查看版本号")
	flag.Parse()
	if *version {
		fmt.Println("Version:", "1.3.2")
		os.Exit(0)
	}
	if c.FFMpegPath == "" {
		c.GetFFmpegPath()
	}
	if c.CachePath == "" {
		c.GetCachePath()
	}
	c.Overlay = "-n"
	if *overlay {
		c.Overlay = "-y"
	}
}

func (c *Config) Composition(videoFile, audioFile, outputFile string) error {
	// 构建FFmpeg命令行参数
	args := []string{
		"-i", videoFile,
		"-i", audioFile,
		"-c:v", "copy", // video不指定编解码，使用bilibili原有编码
		"-c:a", "copy", // audio不指定编解码，使用bilibili原有编码
		"-strict", "experimental", // 宽松编码控制器
		c.Overlay, // 是否覆盖已存在视频
		outputFile,
		"-hide_banner", // 隐藏版本信息和版权声明
		"-stats",       // 只显示统计信息
	}

	//logrus.Info(c.FFMpegPath, args)
	cmd := exec.Command(c.FFMpegPath, args...)

	// 设置输出和错误流 pipe
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	// 启动命令
	if err := cmd.Start(); err != nil {
		c.MessageBox(fmt.Sprintf("执行FFmpeg命令失败: %s", err))
		os.Exit(1)
	}

	// 读取并打印输出流
	go printOutput(stdout)

	// 读取并打印错误流
	go printError(stderr, outputFile)

	assFile := strings.ReplaceAll(outputFile, filepath.Ext(outputFile), conver.AssSuffix)
	if err := copyFile(c.AssPath, assFile, func(*os.File) {}); err != nil {
		logrus.Error(err)
	}
	// 等待命令执行完成
	if err := cmd.Wait(); err == nil {
		fmt.Println()
		logrus.Info("已合成视频文件:", filepath.Base(outputFile))
	}
	return nil
}

func (c *Config) FindM4sFiles(src string, info os.DirEntry, err error) error {
	if err != nil {
		return err
	}
	// 查找.m4s文件
	if filepath.Ext(info.Name()) == conver.M4sSuffix {
		var dst string
		if videoId, audioId := GetVAId(src); videoId != "" && audioId != "" {
			if strings.Contains(info.Name(), audioId) { // 音频文件
				dst = strings.ReplaceAll(src, conver.M4sSuffix, conver.AudioSuffix)
			} else {
				dst = strings.ReplaceAll(src, conver.M4sSuffix, conver.VideoSuffix)
			}
		}
		if err = M4sToAV(src, dst); err != nil {
			c.MessageBox(fmt.Sprintf("%v 转换异常：%v", src, err))
			return err
		}
		logrus.Info("已将m4s转换为音视频文件:", dst)
	}
	return nil
}

func GetCacheDir(cachePath string) ([]string, error) {
	var dirs []string
	err := filepath.Walk(cachePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && path != cachePath {
			if !strings.Contains(path, "output") {
				dirs = append(dirs, path)
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	return dirs, nil
}

func joinUrl(cid string) string {
	//return "https://api.bilibili.com/x/v1/dm/list.so?oid=" + cid
	return "https://comment.bilibili.com/" + cid + conver.XmlSuffix
}

// GetAudioAndVideo 从给定的缓存路径中查找音频和视频文件，并尝试下载并转换xml弹幕为ass格式
// 参数:
// - cachePath: 缓存路径，用于搜索音频、视频文件以及存储下载的弹幕文件
// 返回值:
// - video: 查找到的视频文件路径
// - audio: 查找到的音频文件路径
// - error: 在搜索、下载或转换过程中遇到的任何错误
func (c *Config) GetAudioAndVideo(cachePath string) (string, string, error) {
	var video string
	var audio string

	// 遍历给定路径下的所有文件和目录
	err := filepath.Walk(cachePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err // 如果遇到错误，立即返回
		}
		if !info.IsDir() {
			// 如果是文件，检查是否为视频或音频文件
			if strings.Contains(path, conver.VideoSuffix) {
				video = path // 找到视频文件
			}
			if strings.Contains(path, conver.AudioSuffix) {
				audio = path // 找到音频文件
			}
		} else {
			// 如果是目录，尝试下载并转换xml弹幕为ass格式
			if !c.AssOFF {
				xmlPath := filepath.Join(path, info.Name()+conver.XmlSuffix)
				if e := DownloadFile(joinUrl(info.Name()), xmlPath); e != nil {
					logrus.Warn("XML弹幕下载失败:", err) // 记录下载失败的日志
					return nil
				}
				c.AssPath = conver.Xml2ass(xmlPath) // 转换xml弹幕文件为ass格式
			}
		}
		return nil
	})

	if err != nil {
		return "", "", err // 如果遍历过程中发生错误，返回错误信息
	}

	return video, audio, nil // 返回找到的视频和音频文件路径
}

func copyFile(src, dst string, fn func(*os.File)) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()
	fn(srcFile)

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// 复制文件内容
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}

	return nil
}

func M4sToAV(src, dst string) error {
	return copyFile(src, dst, func(srcFile *os.File) {
		// 读取前9个字符
		data := make([]byte, 9)
		_, _ = io.ReadAtLeast(srcFile, data, 9)
		if string(data) != "000000000" {
			logrus.Errorf("音视频文件不是9个0的头，跳过转换")
			return
		}
		// 移动到第9个字节
		_, err := srcFile.Seek(9, 0) // 从文件开头偏移
		if err != nil {
			logrus.Errorf("文件字节偏移失败: %v", err)
		}
	})
}

// GetCachePath 获取用户视频缓存路径
func (c *Config) GetCachePath() {
	u, err := user.Current()
	if err != nil {
		c.MessageBox(fmt.Sprintf("无法获取当前用户：%v", err))
		return
	}

	videosDir := filepath.Join(u.HomeDir, "Videos", "bilibili")
	if findM4sFiles(videosDir) != nil {
		c.MessageBox("未使用 bilibili 默认缓存路径 " + videosDir + ",\n请选择 bilibili 当前设置的缓存路径！")
		c.SelectDirectory()
		return
	}
	c.CachePath = videosDir
	logrus.Info("选择的 bilibili 缓存目录为: ", c.CachePath)
	return

}

// 查找 m4s 文件
func findM4sFiles(directory string) error {
	var m4sFiles []string
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == conver.M4sSuffix {
			m4sFiles = append(m4sFiles, path)
			return nil
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(m4sFiles) == 0 {
		return fmt.Errorf("找不到缓存目录: %s", directory)
	}
	return nil
}

// GetFFmpegPath 获取 ffmpeg 路径
func (c *Config) GetFFmpegPath() {
	wd, _ := os.Getwd()
	c.FFMpegPath = filepath.Join(wd, FFmpegName) // 指定ffmpeg路径
	if !Exist(c.FFMpegPath) {
		logrus.Info("第一次运行,自动释放ffmpeg.exe")
		if err := DecFile(); err != nil {
			logrus.Error(err)
		}
	}
	if !c.FileHashCompare() {
		logrus.Info("文件不完整,重新释放ffmpeg.exe")
		if err := DecFile(); err != nil {
			logrus.Error(err)
			return
		}
	}
}

// DecFile 解压ffmpeg.exe
func DecFile() error {
	file, err := ffmpegFile.Open(FFmpegName)
	if err != nil {
		return err
	}
	defer file.Close()

	// 使用文件
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	return os.WriteFile(FFmpegName, data, os.ModePerm)
}

func Exist(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		return false
	}
	return true
}

// Filter 过滤文件名
func Filter(name string, err error) string {
	name = strings.ReplaceAll(name, "<", "《")
	name = strings.ReplaceAll(name, ">", "》")
	name = strings.ReplaceAll(name, `\`, "#")
	name = strings.ReplaceAll(name, `"`, "'")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "|", "_")
	name = strings.ReplaceAll(name, "?", "_")
	name = strings.ReplaceAll(name, "*", "_")
	name = strings.ReplaceAll(name, "【", "[")
	name = strings.ReplaceAll(name, "】", "]")
	name = strings.ReplaceAll(name, ":", "：")
	name = strings.TrimSpace(name)

	return name
}

func (c *Config) PanicHandler() {
	if e := recover(); e != nil {
		c.File.Close()
		fmt.Print("按回车键退出...")
		fmt.Scanln()
	}
}

func (c *Config) FileHashCompare() bool {
	file, err := os.ReadFile(c.FFMpegPath)
	if err != nil {
		logrus.Error("打开文件失败:", err)
		return false
	}

	// 计算文件的SHA-256哈希值
	hash := sha256.Sum256(file)
	sha256Str := fmt.Sprintf("%x", hash)

	return FileHashValue == sha256Str
}

func _TEXT(str string) *uint16 {
	ptr, _ := syscall.UTF16PtrFromString(str)
	return ptr
}

func (c *Config) MessageBox(text string) {
	logrus.Error(text)
	win.MessageBox(win.HWND_TOP, _TEXT(text), _TEXT("消息"), win.MB_ICONWARNING)
}

// SelectDirectory 选择bilimini缓存目录
func (c *Config) SelectDirectory() {
	var bsi win.BROWSEINFO
	bsi.LpszTitle = _TEXT("请选择 bilibili 缓存目录")

	pid := win.SHBrowseForFolder(&bsi)
	if pid == 0 {
		logrus.Warn("关闭对话框后自动退出程序")
		os.Exit(1)
	}

	defer win.CoTaskMemFree(pid)

	path := make([]uint16, win.MAX_PATH)
	win.SHGetPathFromIDList(pid, &path[0])

	c.CachePath = syscall.UTF16ToString(path)
	if Exist(filepath.Join(c.CachePath, conver.VideoInfoSuffix)) ||
		Exist(filepath.Join(c.CachePath, conver.VideoInfoJson)) ||
		Exist(filepath.Join(c.CachePath, "load_log")) {
		logrus.Info("选择的 bilibili 缓存目录为:", c.CachePath)
		return
	}
	c.MessageBox("选择的 bilibili 缓存目录不正确，请重新选择！")
	c.SelectDirectory()
}

// LockMutex windows下的单实例锁
func (c *Config) LockMutex(name string) error {
	_, err := windows.CreateMutex(nil, true, _TEXT(name))
	if err != nil {
		return err
	}
	return nil
}

func printOutput(stdout io.ReadCloser) {
	buf := make([]byte, 1024)
	for {
		n, e := stdout.Read(buf)
		if e != nil {
			//logrus.Error("读取标准输出错误:", e)
			return
		}
		if n > 0 {
			fmt.Print(string(buf[:n]))
		}
	}
}

func printError(stderr io.ReadCloser, outputFile string) {
	fmt.Println("准备合成:", filepath.Base(outputFile))
	buf := make([]byte, 1024)
	for {
		n, e := stderr.Read(buf)
		if e != nil {
			//logrus.Error("读取标准错误输出错误:", e)
			return
		}
		if n > 0 {
			cmdErr := string(buf[:n])
			if strings.Contains(cmdErr, "exists") {
				logrus.Warn("跳过已经存在的音视频文件:", filepath.Base(outputFile))
			}
		}
	}
}

// GetVAId 返回.playurl文件中视频文件或音频文件件数组
func GetVAId(patch string) (videoID string, audioID string) {
	pu := filepath.Join(filepath.Dir(patch), conver.PlayUrlSuffix)
	puDate, e := os.ReadFile(pu)
	if e != nil {
		logrus.Error("找不到.playurl文件: ", pu)
		return
	}
	var p conver.PlayUrl
	if err := json.Unmarshal(puDate, &p); err != nil {
		logrus.Error("解析.playurl文件失败: ", err)
		return
	}

	return strconv.Itoa(p.Data.Dash.Video[0].ID), strconv.Itoa(p.Data.Dash.Audio[0].ID)
}

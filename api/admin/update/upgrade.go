package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/utils"
)

const GitHubAPIURL = "https://api.github.com/repos/komari-monitor/komari/releases/latest"

type VersionInfo struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion string `json:"latest_version"`
	DownloadURL   string `json:"download_url"`
	ReleaseNotes  string `json:"release_notes,omitempty"`
	NeedUpgrade   bool   `json:"need_upgrade"`
	CheckedAt     string `json:"checked_at"`
}

type UpgradeResult struct {
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	NewVersion   string `json:"new_version,omitempty"`
	BackupPath   string `json:"backup_path,omitempty"`
}

// GetCurrentVersion 返回当前版本信息
func GetCurrentVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version":      utils.CurrentVersion,
		"version_hash": utils.VersionHash,
	})
}

// GetLatestVersion 查询 GitHub 最新版本
func GetLatestVersion(c *gin.Context) {
	info, err := checkLatestVersion()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("检查版本失败: %v", err),
		})
		return
	}
	c.JSON(http.StatusOK, info)
}

// checkLatestVersion 检查 GitHub 最新版本
func checkLatestVersion() (*VersionInfo, error) {
	info := &VersionInfo{
		CurrentVersion: utils.CurrentVersion,
		CheckedAt:      time.Now().Format(time.RFC3339),
	}

	// 检测架构
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	case "386":
		arch = "386"
	default:
		arch = runtime.GOARCH
	}

	osName := "linux"
	if runtime.GOOS == "windows" {
		osName = "windows"
	}

	// 创建 HTTP 客户端
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// 调用 GitHub API
	req, err := http.NewRequest("GET", GitHubAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Komari-Monitor")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub API 失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回错误状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %v", err)
	}

	// 解析 JSON
	var release struct {
		TagName    string `json:"tag_name"`
		Body       string `json:"body"`
		HTMLURL    string `json:"html_url"`
		Assets     []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}

	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("解析 JSON 失败: %v", err)
	}

	// 清理版本号 (去掉 v 前缀)
	info.LatestVersion = strings.TrimPrefix(release.TagName, "v")
	info.ReleaseNotes = release.Body

	// 构建下载 URL
	fileName := fmt.Sprintf("komari-%s-%s", osName, arch)
	if runtime.GOOS == "windows" {
		fileName += ".exe"
	}
	info.DownloadURL = fmt.Sprintf("https://github.com/komari-monitor/komari/releases/latest/download/%s", fileName)

	// 查找对应架构的下载链接
	for _, asset := range release.Assets {
		expectedName := fileName
		if runtime.GOOS == "windows" && !strings.HasSuffix(asset.Name, ".exe") {
			expectedName += ".exe"
		}
		if asset.Name == expectedName || strings.Contains(asset.Name, fmt.Sprintf("-%s-", arch)) || strings.Contains(asset.Name, fmt.Sprintf("-%s.", arch)) {
			info.DownloadURL = asset.BrowserDownloadURL
			break
		}
	}

	// 比较版本
	info.NeedUpgrade = info.LatestVersion != info.CurrentVersion && info.CurrentVersion != "0.0.1"

	return info, nil
}

// DoUpgrade 执行升级
func DoUpgrade(c *gin.Context) {
	var req struct {
		DownloadURL string `json:"download_url" binding:"required"`
		Version     string `json:"version" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "缺少必要参数",
		})
		return
	}

	result := &UpgradeResult{
		Success: false,
	}

	// 获取当前可执行文件路径
	currentPath, err := os.Executable()
	if err != nil {
		// 尝试使用默认路径
		currentPath = "/opt/komari/komari"
	}

	// 创建备份
	backupPath := currentPath + ".backup." + time.Now().Format("20060102_150405")
	if err := copyFile(currentPath, backupPath); err != nil {
		result.Message = fmt.Sprintf("备份失败: %v", err)
		c.JSON(http.StatusInternalServerError, result)
		return
	}
	result.BackupPath = backupPath

	// 下载新版本
	tmpPath := currentPath + ".new"
	if err := downloadFile(req.DownloadURL, tmpPath); err != nil {
		// 下载失败，恢复备份
		result.Message = fmt.Sprintf("下载失败: %v", err)
		c.JSON(http.StatusInternalServerError, result)
		return
	}

	// 替换文件
	if err := os.Rename(tmpPath, currentPath); err != nil {
		result.Message = fmt.Sprintf("替换文件失败: %v", err)
		c.JSON(http.StatusInternalServerError, result)
		return
	}

	// 设置执行权限
	if err := os.Chmod(currentPath, 0755); err != nil {
		result.Message = fmt.Sprintf("设置权限失败: %v", err)
		c.JSON(http.StatusInternalServerError, result)
		return
	}

	result.Success = true
	result.Message = "下载完成，服务将在重启后使用新版本"
	result.NewVersion = req.Version

	// 注意：实际的重启需要通过 systemd 或其他方式完成
	// 这里只是下载和替换文件，重启由前端触发

	c.JSON(http.StatusOK, result)
}

// RestartService 重启服务
func RestartService(c *gin.Context) {
	// 检查 systemd 是否可用
	cmd := exec.Command("systemctl", "is-active", "komari")
	if err := cmd.Run(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "systemd 服务不可用，请手动重启",
		})
		return
	}

	// 执行重启
	restartCmd := exec.Command("systemctl", "restart", "komari")
	if err := restartCmd.Run(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("重启失败: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "服务正在重启",
	})
}

// copyFile 复制文件
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}

// downloadFile 下载文件
func downloadFile(url, destPath string) error {
	client := &http.Client{
		Timeout: 300 * time.Second, // 5分钟超时
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败，状态码: %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

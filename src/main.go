package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type NetStats struct {
	ReceiveBytes  uint64 `json:"receive_bytes"`
	TransmitBytes uint64 `json:"transmit_bytes"`
}

type Statistics struct {
	TotalReceive  uint64 `json:"total_receive"`
	TotalTransmit uint64 `json:"total_transmit"`
	LastReceive   uint64 `json:"last_receive"`
	LastTransmit  uint64 `json:"last_transmit"`
	LastReset     string `json:"last_reset"` // 新增字段，用于存储上次重置的时间
}

type Comparison struct {
	Category  string  `json:"category"`  // 比较的种类
	Limit     float64 `json:"limit"`     // 上限值
	Threshold float64 `json:"threshold"` // 阈值
	Ratio     float64 `json:"ratio"`     // 比率
}

type TelegramMessage struct {
	ThresholdStatus bool   `json:"threshold_status"`
	RatioStatus     bool   `json:"ratio_status"`
	Token           string `json:"token"`
	ChatID          string `json:"chat_id"`
}

type GotifyMessage struct {
	ThresholdStatus bool   `json:"threshold_status"`
	RatioStatus     bool   `json:"ratio_status"`
	URL             string `json:"url"`
	AppToken        string `json:"app_token"`
}

type Message struct {
	Service  string          `json:"service"`
	Telegram TelegramMessage `json:"telegram"`
	Gotify   GotifyMessage   `json:"gotify"`
}

type Config struct {
	Device     string     `json:"device"`
	Interface  string     `json:"interface"`
	Interval   int        `json:"interval"`
	StartDay   int        `json:"start_day"` // 统计起始日期
	Statistics Statistics `json:"statistics"`
	Comparison Comparison `json:"comparison"`
	Message    Message    `json:"message"`
}

const bytesToGB = 1024 * 1024 * 1024

// Read the /proc/net/dev file to get network statistics for a specific interface
func readNetworkStats(iface string) (NetStats, error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return NetStats{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, iface+":") {
			fields := strings.Fields(line)
			receiveBytes, _ := strconv.ParseUint(fields[1], 10, 64)
			transmitBytes, _ := strconv.ParseUint(fields[9], 10, 64)

			return NetStats{ReceiveBytes: receiveBytes, TransmitBytes: transmitBytes}, nil
		}
	}

	return NetStats{}, fmt.Errorf("interface %s not found", iface)
}

// LoadConfig loads the config from the JSON file
func loadConfig(configFilePath string) (Config, error) {
	var config Config
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return config, nil // Return default config if the file doesn't exist
		}
		return config, err
	}
	err = json.Unmarshal(data, &config)
	return config, err
}

// SaveConfig saves the config to the JSON file
func saveConfig(configFilePath string, config Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configFilePath, data, 0644)
}

// Check if the statistics need to be reset based on the start_day and current date
func checkReset(config *Config) bool {
	currentTime := time.Now()

	// Parse the last reset time from the config
	lastReset, err := time.Parse("2006-01-02", config.Statistics.LastReset)
	if err != nil {
		// If there's an error parsing the last reset, assume we need to reset
		return true
	}

	// Calculate the number of days in the current month
	firstOfMonth := time.Date(currentTime.Year(), currentTime.Month(), 1, 0, 0, 0, 0, time.Local)
	nextMonth := firstOfMonth.AddDate(0, 1, 0)          // First day of next month
	lastDayOfMonth := nextMonth.AddDate(0, 0, -1).Day() // Get the last day of current month

	// If start_day is greater than the last day of this month, adjust it to the last day
	resetDay := config.StartDay
	if resetDay > lastDayOfMonth {
		resetDay = lastDayOfMonth
	}

	// Calculate the reset date for the current month
	resetDate := time.Date(currentTime.Year(), currentTime.Month(), resetDay, 0, 0, 0, 0, time.Local)

	// If the last reset was before the current reset date and now is after or on the reset date, reset statistics
	if lastReset.Before(resetDate) && currentTime.After(resetDate) {
		return true
	}

	return false
}

// 发送统计摘要信息
func sendStatisticsSummary(config *Config) error {
	// 计算总流量（GB）
	receiveGB := float64(config.Statistics.TotalReceive) / bytesToGB
	transmitGB := float64(config.Statistics.TotalTransmit) / bytesToGB
	totalGB := receiveGB + transmitGB

	// 计算使用率
	var usagePercent float64
	categoryUsage := "未知"

	switch config.Comparison.Category {
	case "download":
		usagePercent = receiveGB / config.Comparison.Limit * 100
		categoryUsage = fmt.Sprintf("下载流量：%.2f GB (%.1f%%)", receiveGB, usagePercent)
	case "upload":
		usagePercent = transmitGB / config.Comparison.Limit * 100
		categoryUsage = fmt.Sprintf("上传流量：%.2f GB (%.1f%%)", transmitGB, usagePercent)
	case "upload+download":
		usagePercent = totalGB / config.Comparison.Limit * 100
		categoryUsage = fmt.Sprintf("总流量：%.2f GB (%.1f%%)", totalGB, usagePercent)
	case "anymax":
		maxGB := max(receiveGB, transmitGB)
		usagePercent = maxGB / config.Comparison.Limit * 100
		categoryUsage = fmt.Sprintf("最大单向流量：%.2f GB (%.1f%%)", maxGB, usagePercent)
	}

	// 上次重置时间
	lastResetTime, _ := time.Parse("2006-01-02", config.Statistics.LastReset)

	// 构建消息
	message := fmt.Sprintf(
		"周期统计摘要 (%s 至今):\n\n下载流量：%.2f GB\n上传流量：%.2f GB\n合计流量：%.2f GB\n\n计费方式：%s\n限额：%.2f GB\n%s",
		lastResetTime.Format("2006-01-02"),
		receiveGB,
		transmitGB,
		totalGB,
		config.Comparison.Category,
		config.Comparison.Limit,
		categoryUsage,
	)

	// 发送消息
	return sendMessage(config, message)
}

// Reset statistics and also reset the Telegram status flags
func resetStatistics(config *Config, configFilePath string) {
	// 在重置之前发送统计摘要
	err := sendStatisticsSummary(config)
	if err != nil {
		fmt.Printf("Failed to send statistics summary: %v\n", err)
	}

	// Reset statistics
	config.Statistics.TotalReceive = 0
	config.Statistics.TotalTransmit = 0

	// Reset the last reset date
	config.Statistics.LastReset = time.Now().Format("2006-01-02")

	// Reset Telegram status flags
	config.Message.Telegram.ThresholdStatus = false
	config.Message.Telegram.RatioStatus = false

	// Reset Gotify status flags
	config.Message.Gotify.ThresholdStatus = false
	config.Message.Gotify.RatioStatus = false

	// Save the reset config
	err = saveConfig(configFilePath, *config)
	if err != nil {
		fmt.Printf("Failed to save config after reset in resetStatistics: %v\n", err)
	}
}

// Send a message to Telegram via Bot API
func sendTelegramMessage(token, chatID, message, device string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	body := map[string]string{
		"chat_id": chatID,
		"text":    fmt.Sprintf("[%s] %s", device, message),
	}
	jsonBody, _ := json.Marshal(body)

	_, err := http.Post(url, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to send message to Telegram: %v", err)
	}

	return nil
}

// Send a message to Gotify server
func sendGotifyMessage(url, appToken, message, device string) error {
	apiURL := fmt.Sprintf("%s/message", strings.TrimRight(url, "/"))

	body := map[string]string{
		"title":    fmt.Sprintf("Network Monitor: %s", device),
		"message":  message,
		"priority": "5",
	}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request to Gotify: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", appToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send message to Gotify: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("got error status from Gotify: %s", resp.Status)
	}

	return nil
}

// Send message using the configured service
func sendMessage(config *Config, message string) error {
	switch config.Message.Service {
	case "telegram":
		return sendTelegramMessage(
			config.Message.Telegram.Token,
			config.Message.Telegram.ChatID,
			message,
			config.Device,
		)
	case "gotify":
		return sendGotifyMessage(
			config.Message.Gotify.URL,
			config.Message.Gotify.AppToken,
			message,
			config.Device,
		)
	default:
		return fmt.Errorf("unknown message service: %s", config.Message.Service)
	}
}

// Check if a command exists in the system
func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// Perform comparison based on category and thresholds
func performComparison(config *Config, configFilePath string) error {
	var valueInGB float64

	switch config.Comparison.Category {
	case "download":
		valueInGB = float64(config.Statistics.TotalReceive) / bytesToGB
	case "upload":
		valueInGB = float64(config.Statistics.TotalTransmit) / bytesToGB
	case "upload+download":
		valueInGB = float64(config.Statistics.TotalReceive+config.Statistics.TotalTransmit) / bytesToGB
	case "anymax":
		// 选择上传和下载中较大的值
		receiveGB := float64(config.Statistics.TotalReceive) / bytesToGB
		transmitGB := float64(config.Statistics.TotalTransmit) / bytesToGB
		valueInGB = max(receiveGB, transmitGB)
	default:
		return fmt.Errorf("invalid comparison category: %s", config.Comparison.Category)
	}

	thresholdLimit := config.Comparison.Limit * config.Comparison.Threshold
	ratioLimit := config.Comparison.Limit * config.Comparison.Ratio

	// Compare with threshold and send message if needed
	var thresholdStatus, ratioStatus bool
	if config.Message.Service == "telegram" {
		thresholdStatus = config.Message.Telegram.ThresholdStatus
		ratioStatus = config.Message.Telegram.RatioStatus
	} else if config.Message.Service == "gotify" {
		thresholdStatus = config.Message.Gotify.ThresholdStatus
		ratioStatus = config.Message.Gotify.RatioStatus
	}

	// Compare with threshold and send message if needed
	if valueInGB >= thresholdLimit && !thresholdStatus {
		message := fmt.Sprintf("流量提醒：当前使用量为 %.2f GB，超过了设置的%.0f%%阈值", valueInGB, config.Comparison.Threshold*100)
		err := sendMessage(config, message)
		if err != nil {
			fmt.Printf("Failed to send threshold message: %v\n", err)
		} else {
			// Update status based on selected service
			if config.Message.Service == "telegram" {
				config.Message.Telegram.ThresholdStatus = true
			} else if config.Message.Service == "gotify" {
				config.Message.Gotify.ThresholdStatus = true
			}

			// Save the updated config to the file
			err = saveConfig(configFilePath, *config)
			if err != nil {
				fmt.Printf("Failed to save config after threshold message: %v\n", err)
			}
		}
	}

	// Check for shutdown warning and send message if needed
	if valueInGB >= ratioLimit && !ratioStatus {
		message := fmt.Sprintf("关机警告：当前使用量 %.2f GB，超过了限制的%.0f%%，即将关机！", valueInGB, config.Comparison.Ratio*100)
		err := sendMessage(config, message)
		if err != nil {
			fmt.Printf("Failed to send ratio warning message: %v\n", err)
		} else {
			// Update status based on selected service
			if config.Message.Service == "telegram" {
				config.Message.Telegram.RatioStatus = true
			} else if config.Message.Service == "gotify" {
				config.Message.Gotify.RatioStatus = true
			}

			// Save the updated config to the file
			err = saveConfig(configFilePath, *config)
			if err != nil {
				fmt.Printf("Failed to save config after ratio warning: %v\n", err)
			}

			// Wait for 30 seconds before shutting down
			time.Sleep(30 * time.Second)

			// Check if shutdown command exists, otherwise use poweroff
			var cmd *exec.Cmd
			if commandExists("shutdown") {
				cmd = exec.Command("shutdown", "-h", "now")
			} else {
				cmd = exec.Command("poweroff")
			}

			err := cmd.Run()
			if err != nil {
				fmt.Printf("Failed to execute shutdown command: %v\n", err)
			}
		}
	}

	return nil
}

func main() {
	// Parse the command-line flag for the config file path
	configFilePath := flag.String("c", "/path/to/config.json", "Path to the config JSON file")
	flag.Parse()

	// Load the config file (or create a new one if not exists)
	config, err := loadConfig(*configFilePath)
	if err != nil {
		fmt.Printf("Failed to load config in main: %v\n", err)
		return
	}

	// Set the interface name (if not already set in config)
	if config.Interface == "" {
		config.Interface = "eth0" // Default to eth0, you can change it or make it configurable
	}

	// Check if the interface exists
	_, err = readNetworkStats(config.Interface)
	if err != nil {
		fmt.Printf("Error checking interface existing: %v\n", err)
		return
	}

	// Use the interval defined in config.json
	interval := config.Interval
	if interval == 0 {
		interval = 600 // Default to 600 seconds if not specified
	}

	for {
		// Check if the statistics need to be reset based on the start day
		if checkReset(&config) {
			//resetStatistics(&config) // Reset statistics and telegram statuses
			resetStatistics(&config, *configFilePath)
		}

		stats, err := readNetworkStats(config.Interface)
		if err != nil {
			fmt.Printf("Error reading network stats: %v\n", err)
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}

		// Check for system reboot by comparing previous and current values
		if stats.ReceiveBytes < config.Statistics.LastReceive {
			// System reboot detected for receive bytes
			config.Statistics.TotalReceive += config.Statistics.LastReceive
		}
		if stats.TransmitBytes < config.Statistics.LastTransmit {
			// System reboot detected for transmit bytes
			config.Statistics.TotalTransmit += config.Statistics.LastTransmit
		}

		// Update the total counts
		config.Statistics.TotalReceive += stats.ReceiveBytes - config.Statistics.LastReceive
		config.Statistics.TotalTransmit += stats.TransmitBytes - config.Statistics.LastTransmit

		// Save the current stats as the "last" stats for the next check
		config.Statistics.LastReceive = stats.ReceiveBytes
		config.Statistics.LastTransmit = stats.TransmitBytes

		// Save the updated config to the file
		err = saveConfig(*configFilePath, config)
		if err != nil {
			fmt.Printf("Failed to update stats to config: %v\n", err)
		}

		// Print the stats, in GB units for better readability
		// fmt.Printf("Total Receive: %.2f GB, Total Transmit: %.2f GB\n",
		// 	float64(config.Statistics.TotalReceive)/bytesToGB,
		// 	float64(config.Statistics.TotalTransmit)/bytesToGB)

		// Perform comparison and check for warnings
		//err = performComparison(&config)
		err = performComparison(&config, *configFilePath)
		if err != nil {
			fmt.Printf("Comparison error: %v\n", err)
		}

		// Wait for the next interval
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

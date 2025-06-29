package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.bug.st/serial"
	"gopkg.in/yaml.v3"
)

const (
	DefaultConfigPath   = "./config.yml"
	DefaultFanSpeed     = 50
	DefaultBaudRate     = 115200
	DefaultUpdateRateMs = 500
	DefaultMinFanSpeed  = 20
	DefaultMaxFanSpeed  = 100
	DefaultThermBValue  = 3950
	FanSpeedStepSize    = 5
)

// JSON Schema Types
type Command struct {
	Cmd     string      `json:"cmd"`
	Payload interface{} `json:"payload,omitempty"`
}

type SetFanSpeedPayload struct {
	Fan   int `json:"fan"`
	Speed int `json:"speed"`
}

type SetThConfigPayload struct {
	Th     int `json:"th"`
	BValue int `json:"b-value"`
}

type Response struct {
	Status       string               `json:"status,omitempty"`
	Msg          string               `json:"msg,omitempty"`
	Fans         map[string]FanData   `json:"fans,omitempty"`
	Thermometers map[string]ThermData `json:"thermometers,omitempty"`
}

type FanData struct {
	Speed int `json:"speed"`
	RPM   int `json:"rpm"`
}

type ThermData struct {
	Temp   float64 `json:"temp"`
	BValue int     `json:"b-value"`
}

// Configuration Types
type Config struct {
	Server      ServerConfig            `yaml:"server"`
	Fans        map[string]FanConfig    `yaml:"fans"`
	Thermistors map[string]ThermConfig  `yaml:"thermistors"`
	Curves      map[string][]CurvePoint `yaml:"curves"`
}

type ServerConfig struct {
	SocketPath   string        `yaml:"socket_path"`
	UpdateRate   time.Duration `yaml:"update_rate"`
	SerialDevice string        `yaml:"serial_device"`
	BaudRate     int           `yaml:"baud_rate"`
	AutoDetect   bool          `yaml:"auto_detect"`
}

type FanConfig struct {
	Thermistor    string `yaml:"thermistor"`
	Curve         string `yaml:"curve"`
	MinSpeed      int    `yaml:"min_speed"`
	MaxSpeed      int    `yaml:"max_speed"`
	Interpolation bool   `yaml:"interpolation"`
}

type ThermConfig struct {
	BValue int `yaml:"b_value"`
}

type CurvePoint struct {
	Temp  float64 `yaml:"temp"`
	Speed int     `yaml:"speed"`
}

// Controller State
type Controller struct {
	config       *Config
	serialPort   serial.Port
	measurements Response
	detached     bool
}

var (
	// Platform-specific socket paths
	socketPath = getPlatformSocketPath()
)

func getPlatformSocketPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/tmp/fancontroller.sock"
	case "linux":
		return "/run/fancontroller.sock"
	case "freebsd":
		return "/var/run/fancontroller.sock"
	default:
		return "/tmp/fancontroller.sock"
	}
}

func main() {
	var (
		configPath = flag.String("config", DefaultConfigPath, "Path to config file")
		daemon     = flag.Bool("daemon", false, "Run as daemon")
		detach     = flag.Bool("detach", false, "Detach from automatic control")
		show       = flag.Bool("show", false, "Show current measurements")
		listSerial = flag.Bool("list-serial", false, "List available serial devices")
		help       = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		printHelp()
		return
	}

	if *listSerial {
		listSerialDevices()
		return
	}

	// Check if daemon is already running
	running := isDaemonRunning()

	if *show {
		if !running {
			fmt.Println("Daemon is not running")
			os.Exit(1)
		}
		showMeasurements()
		return
	}

	if *detach {
		if !running {
			fmt.Println("Daemon is not running")
			os.Exit(1)
		}
		sendDetachCommand()
		return
	}

	if *daemon || !running {
		// Load configuration
		config, err := loadConfig(*configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}

		// Run daemon
		runDaemon(config)
	} else {
		fmt.Println("Daemon is already running. Use -show to see measurements or -detach to disable automatic control.")
	}
}

func printHelp() {
	fmt.Println("Fan Controller Host")
	fmt.Println("")
	fmt.Println("Usage:")
	fmt.Println("  fancontroller [options]")
	fmt.Println("")
	fmt.Println("Options:")
	fmt.Println("  -config string     Path to config file (default \"./config.yml\")")
	fmt.Println("  -daemon           Force daemon mode")
	fmt.Println("  -detach           Detach from automatic control")
	fmt.Println("  -show             Show current measurements")
	fmt.Println("  -list-serial      List available serial devices")
	fmt.Println("  -help             Show this help")
	fmt.Println("")
	fmt.Println("If no daemon is running, it will start automatically.")
	fmt.Println("")
	fmt.Println("Platform support: macOS, Linux, FreeBSD")
	fmt.Println("Windows support available via Docker with serial device passthrough")
}

func listSerialDevices() {
	fmt.Println("Available serial devices:")
	fmt.Println("========================")

	devices := findSerialDevices()
	if len(devices) == 0 {
		fmt.Println("No serial devices found")
		return
	}

	for _, device := range devices {
		fmt.Printf("  %s\n", device)
	}
}

func findSerialDevices() []string {
	var devices []string
	var searchPaths []string

	switch runtime.GOOS {
	case "darwin":
		// macOS
		searchPaths = []string{
			"/dev/tty.usb*",
			"/dev/tty.usbserial*",
			"/dev/tty.usbmodem*",
			"/dev/cu.usb*",
			"/dev/cu.usbserial*",
			"/dev/cu.usbmodem*",
		}
	case "linux":
		// Linux
		searchPaths = []string{
			"/dev/ttyUSB*",
			"/dev/ttyACM*",
			"/dev/ttyS*",
		}
	case "freebsd":
		// FreeBSD
		searchPaths = []string{
			"/dev/cuaU*",
			"/dev/ttyU*",
			"/dev/cuau*",
			"/dev/ttyu*",
		}
	default:
		// Generic Unix
		searchPaths = []string{
			"/dev/tty*",
			"/dev/cu*",
		}
	}

	// Search for devices using glob patterns
	for _, pattern := range searchPaths {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}

		for _, match := range matches {
			// Try to open the device to verify it's accessible
			if p, err := serial.Open(match, &serial.Mode{BaudRate: 9600}); err == nil {
				p.Close()
				devices = append(devices, match)
			}
		}
	}

	return devices
}

func autoDetectSerialDevice() string {
	devices := findSerialDevices()

	// Platform-specific preferences
	var preferred []string
	switch runtime.GOOS {
	case "darwin":
		preferred = []string{"tty.usb", "cu.usb"}
	case "linux":
		preferred = []string{"ttyUSB", "ttyACM"}
	case "freebsd":
		preferred = []string{"cuaU", "ttyU"}
	}

	// Find preferred device
	for _, pref := range preferred {
		for _, device := range devices {
			if strings.Contains(device, pref) {
				return device
			}
		}
	}

	// Return first available device
	if len(devices) > 0 {
		return devices[0]
	}

	return ""
}

func validateConfig(config *Config) error {
	// Validate fans reference existing thermistors and curves
	for fanName, fanConfig := range config.Fans {
		if _, exists := config.Thermistors[fanConfig.Thermistor]; !exists {
			return fmt.Errorf("fan '%s' references non-existent thermistor '%s'", fanName, fanConfig.Thermistor)
		}
		if _, exists := config.Curves[fanConfig.Curve]; !exists {
			return fmt.Errorf("fan '%s' references non-existent curve '%s'", fanName, fanConfig.Curve)
		}
	}
	return nil
}

func loadConfig(path string) (*Config, error) {
	// Create default config if file doesn't exist
	if _, err := os.Stat(path); os.IsNotExist(err) {
		config := createDefaultConfig()
		if err := saveConfig(config, path); err != nil {
			return nil, fmt.Errorf("failed to create default config: %v", err)
		}
		fmt.Printf("Created default config at %s\n", path)
		return config, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	if err := validateConfig(&config); err != nil {
		return nil, fmt.Errorf("config validation failed: %v", err)
	}

	// Set defaults using constants
	if config.Server.SocketPath == "" {
		config.Server.SocketPath = socketPath
	}
	if config.Server.UpdateRate == 0 {
		config.Server.UpdateRate = DefaultUpdateRateMs * time.Millisecond
	}
	if config.Server.BaudRate == 0 {
		config.Server.BaudRate = DefaultBaudRate
	}
	if config.Server.SerialDevice == "" && config.Server.AutoDetect {
		if device := autoDetectSerialDevice(); device != "" {
			config.Server.SerialDevice = device
			log.Printf("Auto-detected serial device: %s", device)
		}
	}

	return &config, nil
}

func createDefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			SocketPath:   socketPath,
			UpdateRate:   DefaultUpdateRateMs * time.Millisecond,
			SerialDevice: "", // Will be auto-detected
			BaudRate:     DefaultBaudRate,
			AutoDetect:   true,
		},
		Fans: map[string]FanConfig{
			"fan1": {
				Thermistor:    "th1",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
			},
			"fan2": {
				Thermistor:    "th1",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
			},
			"fan3": {
				Thermistor:    "th2",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
			},
			"fan4": {
				Thermistor:    "th2",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
			},
			"fan5": {
				Thermistor:    "th3",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
			},
			"fan6": {
				Thermistor:    "th3",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
			},
		},
		Thermistors: map[string]ThermConfig{
			"th1": {BValue: DefaultThermBValue},
			"th2": {BValue: DefaultThermBValue},
			"th3": {BValue: DefaultThermBValue},
		},
		Curves: map[string][]CurvePoint{
			"default": {
				{Temp: 30.0, Speed: 20},
				{Temp: 40.0, Speed: 30},
				{Temp: 50.0, Speed: 50},
				{Temp: 60.0, Speed: 70},
				{Temp: 70.0, Speed: 85},
				{Temp: 80.0, Speed: 100},
			},
			"cool": {
				{Temp: 25.0, Speed: 30},
				{Temp: 35.0, Speed: 50},
				{Temp: 45.0, Speed: 70},
				{Temp: 55.0, Speed: 85},
				{Temp: 65.0, Speed: 100},
			},
			"full-speed": {
				{Temp: 0.0, Speed: 100},
				{Temp: 100.0, Speed: 100},
			},
			"silent": {
				{Temp: 35.0, Speed: 0},
				{Temp: 45.0, Speed: 20},
				{Temp: 55.0, Speed: 30},
				{Temp: 65.0, Speed: 40},
				{Temp: 70.0, Speed: 45},
				{Temp: 80.0, Speed: 50},
				{Temp: 85.0, Speed: 65},
				{Temp: 90.0, Speed: 80},
				{Temp: 95.0, Speed: 100},
			},
		},
	}
}

func saveConfig(config *Config, path string) error {
	// Create directory if it doesn't exist
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

func isDaemonRunning() bool {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func runDaemon(config *Config) {
	controller := &Controller{
		config: config,
	}

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		cancel()
	}()

	// Connect to serial device
	if err := controller.connectToDevice(); err != nil {
		log.Fatalf("Failed to connect to device: %v", err)
	}
	defer controller.serialPort.Close()

	// Configure thermistors
	if err := controller.configureThermistors(); err != nil {
		log.Fatalf("Failed to configure thermistors: %v", err)
	}

	// Start socket server
	go controller.startSocketServer(ctx)

	// Start control loop
	controller.controlLoop(ctx)

	log.Println("Daemon stopped")
}

func (c *Controller) connectToDevice() error {
	if c.config.Server.SerialDevice == "" {
		return fmt.Errorf("no serial device specified and auto-detection failed")
	}

	mode := &serial.Mode{
		BaudRate: c.config.Server.BaudRate,
		Parity:   serial.NoParity,
		DataBits: 8,
		StopBits: serial.OneStopBit,
	}

	port, err := serial.Open(c.config.Server.SerialDevice, mode)
	if err != nil {
		return fmt.Errorf("failed to open serial device %s: %v", c.config.Server.SerialDevice, err)
	}

	c.serialPort = port
	log.Printf("Connected to serial device: %s at %d baud", c.config.Server.SerialDevice, c.config.Server.BaudRate)
	return nil
}

func (c *Controller) configureThermistors() error {
	for thName, thConfig := range c.config.Thermistors {
		thNum, err := strconv.Atoi(thName[2:]) // Extract number from "th1", "th2", etc.
		if err != nil {
			log.Printf("Warning: Invalid thermistor name '%s', skipping configuration", thName)
			continue
		}

		cmd := Command{
			Cmd: "set-th-config",
			Payload: SetThConfigPayload{
				Th:     thNum,
				BValue: thConfig.BValue,
			},
		}

		if err := c.sendCommand(cmd); err != nil {
			return fmt.Errorf("failed to configure %s: %v", thName, err)
		}
	}
	log.Println("Thermistors configured")
	return nil
}

func (c *Controller) startSocketServer(ctx context.Context) {
	// Remove existing socket
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("Failed to create Unix socket: %v", err)
	}

	defer listener.Close()
	defer os.Remove(socketPath)

	log.Printf("Socket server started at %s", socketPath)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		go c.handleClientConnection(conn)
	}
}

func (c *Controller) handleClientConnection(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var cmd Command
	if err := decoder.Decode(&cmd); err != nil {
		encoder.Encode(Response{Status: "error", Msg: "Invalid JSON"})
		return
	}

	switch cmd.Cmd {
	case "get-measurements":
		encoder.Encode(c.measurements)
	case "detach":
		c.detached = true
		encoder.Encode(Response{Status: "ok"})
		log.Println("Detached from automatic control")
	case "attach":
		c.detached = false
		encoder.Encode(Response{Status: "ok"})
		log.Println("Attached to automatic control")
	default:
		encoder.Encode(Response{Status: "error", Msg: "Unknown command"})
	}
}

func (c *Controller) controlLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.Server.UpdateRate)
	defer ticker.Stop()

	log.Println("Control loop started")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Send alive beacon
			if err := c.sendAliveBeacon(); err != nil {
				log.Printf("Failed to send alive beacon: %v", err)
				continue
			}

			if err := c.updateMeasurements(); err != nil {
				log.Printf("Failed to update measurements: %v", err)
				continue
			}

			if !c.detached {
				if err := c.updateFanSpeeds(); err != nil {
					log.Printf("Failed to update fan speeds: %v", err)
				}
			}
		}
	}
}

func (c *Controller) sendAliveBeacon() error {
	cmd := Command{Cmd: "alive"}
	return c.sendCommand(cmd)
}

func (c *Controller) updateMeasurements() error {
	cmd := Command{Cmd: "get-measurements"}
	return c.sendCommand(cmd)
}

func (c *Controller) updateFanSpeeds() error {
	if c.measurements.Thermometers == nil {
		return nil
	}

	for fanName, fanConfig := range c.config.Fans {
		// Get thermistor temperature
		thData, exists := c.measurements.Thermometers[fanConfig.Thermistor]
		if !exists {
			log.Printf("Warning: Thermistor '%s' for fan '%s' not found in measurements", fanConfig.Thermistor, fanName)
			continue
		}

		// Calculate target speed from curve
		targetSpeed := c.calculateSpeedFromCurve(thData.Temp, fanConfig.Curve, fanConfig)

		// Apply min/max limits
		if targetSpeed < fanConfig.MinSpeed {
			targetSpeed = fanConfig.MinSpeed
		}
		if targetSpeed > fanConfig.MaxSpeed {
			targetSpeed = fanConfig.MaxSpeed
		}

		// Round to 5% steps
		targetSpeed = (targetSpeed / FanSpeedStepSize) * FanSpeedStepSize

		// Get current fan speed
		currentSpeed := 0
		if fanData, exists := c.measurements.Fans[fanName]; exists {
			currentSpeed = fanData.Speed
		}

		// Update fan speed if different
		if targetSpeed != currentSpeed {
			fanNum, err := strconv.Atoi(fanName[3:]) // Extract number from "fan1", "fan2", etc.
			if err != nil {
				continue
			}

			cmd := Command{
				Cmd: "set-fan-speed",
				Payload: SetFanSpeedPayload{
					Fan:   fanNum,
					Speed: targetSpeed,
				},
			}

			if err := c.sendCommand(cmd); err != nil {
				log.Printf("Failed to set %s speed: %v", fanName, err)
			} else {
				log.Printf("%s: %.1f°C -> %d%% (was %d%%)", fanName, thData.Temp, targetSpeed, currentSpeed)
			}
		}
	}

	return nil
}

func (c *Controller) calculateSpeedFromCurve(temp float64, curveName string, fanConfig FanConfig) int {
	curve, exists := c.config.Curves[curveName]
	if !exists {
		log.Printf("Warning: Curve '%s' not found, using default speed %d%%", curveName, DefaultFanSpeed)
		return DefaultFanSpeed
	}

	if len(curve) == 0 {
		log.Printf("Warning: Curve '%s' is empty, using default speed %d%%", curveName, DefaultFanSpeed)
		return DefaultFanSpeed
	}

	// Sort curve points by temperature
	sort.Slice(curve, func(i, j int) bool {
		return curve[i].Temp < curve[j].Temp
	})

	// If temperature is below first point, return first speed
	if temp <= curve[0].Temp {
		return curve[0].Speed
	}

	// If temperature is above last point, return last speed
	if temp >= curve[len(curve)-1].Temp {
		return curve[len(curve)-1].Speed
	}

	// Find the appropriate curve segment
	for i := 0; i < len(curve)-1; i++ {
		if temp >= curve[i].Temp && temp <= curve[i+1].Temp {
			if fanConfig.Interpolation {
				// Linear interpolation
				tempRange := curve[i+1].Temp - curve[i].Temp
				speedRange := float64(curve[i+1].Speed - curve[i].Speed)
				tempOffset := temp - curve[i].Temp

				speed := float64(curve[i].Speed) + (tempOffset/tempRange)*speedRange
				return int(speed + 0.5) // Round to nearest integer
			} else {
				// Step function - use lower point until halfway, then upper point
				midTemp := curve[i].Temp + (curve[i+1].Temp-curve[i].Temp)/2
				if temp < midTemp {
					return curve[i].Speed
				} else {
					return curve[i+1].Speed
				}
			}
		}
	}

	log.Printf("Warning: Temperature %.1f°C not found in curve '%s', using default speed %d%%", temp, curveName, DefaultFanSpeed)
	return DefaultFanSpeed
}

func (c *Controller) sendCommand(cmd Command) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	// Send command with newline delimiter
	data = append(data, '\n')
	if _, err := c.serialPort.Write(data); err != nil {
		return err
	}

	// Read response
	scanner := bufio.NewScanner(c.serialPort)
	if !scanner.Scan() {
		return fmt.Errorf("no response from device")
	}

	var response Response
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		return fmt.Errorf("invalid response: %v", err)
	}

	if response.Status == "error" {
		return fmt.Errorf("device error: %s", response.Msg)
	}

	// Update measurements if this was a get-measurements command
	if cmd.Cmd == "get-measurements" {
		c.measurements = response
	}

	return nil
}

func connectToSocket() (net.Conn, error) {
	return net.Dial("unix", socketPath)
}

func showMeasurements() {
	conn, err := connectToSocket()
	if err != nil {
		fmt.Printf("Failed to connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Send get-measurements command
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	cmd := Command{Cmd: "get-measurements"}
	if err := encoder.Encode(cmd); err != nil {
		fmt.Printf("Failed to send command: %v\n", err)
		os.Exit(1)
	}

	var response Response
	if err := decoder.Decode(&response); err != nil {
		fmt.Printf("Failed to read response: %v\n", err)
		os.Exit(1)
	}

	// Display measurements
	fmt.Println("Fan Controller Measurements")
	fmt.Println("===========================")
	fmt.Println()

	if response.Thermometers != nil {
		fmt.Println("Thermistors:")
		for name, data := range response.Thermometers {
			fmt.Printf("  %s: %.1f°C (B-value: %d)\n", name, data.Temp, data.BValue)
		}
		fmt.Println()
	}

	if response.Fans != nil {
		fmt.Println("Fans:")
		for name, data := range response.Fans {
			fmt.Printf("  %s: %d%% (%d RPM)\n", name, data.Speed, data.RPM)
		}
	}
}

func sendDetachCommand() {
	conn, err := connectToSocket()
	if err != nil {
		fmt.Printf("Failed to connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	cmd := Command{Cmd: "detach"}
	if err := encoder.Encode(cmd); err != nil {
		fmt.Printf("Failed to send command: %v\n", err)
		os.Exit(1)
	}

	var response Response
	if err := decoder.Decode(&response); err != nil {
		fmt.Printf("Failed to read response: %v\n", err)
		os.Exit(1)
	}

	if response.Status == "ok" {
		fmt.Println("Successfully detached from automatic control")
	} else {
		fmt.Printf("Failed to detach: %s\n", response.Msg)
		os.Exit(1)
	}
}

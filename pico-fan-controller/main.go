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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.bug.st/serial"
	"go.yaml.in/yaml/v3"
)

const (
	DefaultConfigPath   = "./config.yml"
	DefaultFanSpeed     = 50
	DefaultBaudRate     = 115200
	DefaultUpdateRateMs = 1000
	DefaultMinFanSpeed  = 0
	DefaultMaxFanSpeed  = 100
	DefaultThermBValue  = 3450
	FanSpeedStepSize    = 5
)

type Command struct {
	Cmd  string      `json:"cmd"`
	Data interface{} `json:"data,omitempty"`
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
	Status string      `json:"status,omitempty"`
	Msg    string      `json:"msg,omitempty"`
	Data   interface{} `json:"data,omitempty"`
}

type Measurements struct {
	Fans         map[string]FanData   `json:"fans,omitempty"`
	Thermometers map[string]ThermData `json:"thermometers,omitempty"`
}

type FanData struct {
	Speed     int    `json:"speed"`
	RPM       int    `json:"rpm"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

type ThermData struct {
	Temp      float64 `json:"temp"`
	BValue    int     `json:"b-value"`
	Name      string  `json:"name"`
	Connected bool    `json:"connected"`
}

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
	LogPath      string        `yaml:"log_path"`
}

type FanConfig struct {
	Thermistor    string `yaml:"thermistor"`
	Curve         string `yaml:"curve"`
	MinSpeed      int    `yaml:"min_speed"`
	MaxSpeed      int    `yaml:"max_speed"`
	Interpolation bool   `yaml:"interpolation"`
	Manual        bool   `yaml:"manual"`
	FixedSpeed    int    `yaml:"fixed_speed"`
	Connected     bool   `yaml:"connected"`
	Name          string `yaml:"name"`
}

type ThermConfig struct {
	BValue    int    `yaml:"b_value"`
	Name      string `yaml:"name"`
	Connected bool   `yaml:"connected"`
}

type CurvePoint struct {
	Temp  float64 `yaml:"temp"`
	Speed int     `yaml:"speed"`
}

type Controller struct {
	config       *Config
	configFile   *string
	serialPort   serial.Port
	measurements Measurements
	detached     bool
}

func SortKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func startAsDaemon(configFile string, logFile string) {
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}

	cmd := exec.Command(os.Args[0], "run", "--config", configFile)
	cmd.Stdout = file
	cmd.Stderr = file
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Fully detach
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start daemon: %v", err)
	}

	fmt.Printf("Daemon started with PID %d, logging to %s\n", cmd.Process.Pid, logFile)
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}

	switch os.Args[1] {
	case "run":
		runCmd := flag.NewFlagSet("run", flag.ExitOnError)
		configPath := runCmd.String("config", DefaultConfigPath, "Path to config file")
		daemon := runCmd.Bool("daemon", false, "Run in daemon mode")
		runCmd.Parse(os.Args[2:])
		config, err := loadConfig(*configPath)
		logPath := config.Server.LogPath
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
		if *daemon {
			startAsDaemon(*configPath, logPath)
			return
		}
		startController(config, configPath)

	case "show":
		showCmd := flag.NewFlagSet("show", flag.ExitOnError)
		jsonOutput := showCmd.Bool("json", false, "Output measurements as JSON")
		showCmd.Parse(os.Args[2:])

		if !isDaemonRunning() {
			fmt.Println("Daemon is not running")
			os.Exit(1)
		}
		showMeasurements(*jsonOutput)

	case "list-curves":
		listFanCurves()
		return

	case "set":
		handleSetCommand()
		return

	case "list-serial":
		listSerialDevices()

	case "help":
		printHelp()

	default:
		fmt.Printf("Unknown command: %s\n\n", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println("Pico Fan Controller")
	fmt.Println("")
	fmt.Println("Usage:")
	fmt.Println("  fancontroller <command> [options]")
	fmt.Println("")
	fmt.Println("Commands:")
	fmt.Println("  run               Run fancontroller")
	fmt.Println("    --config        Path to config file (default: ./config.yml)")
	fmt.Println("    --daemon        Run in daemon mode")
	fmt.Println("")
	fmt.Println("  show              Show current measurements")
	fmt.Println("    --json          Output as JSON")
	fmt.Println("")
	fmt.Println("  set fan <ID> <property> <value>")
	fmt.Println("    Properties:")
	fmt.Println("      curve         Set fan curve name")
	fmt.Println("      thermistor    Assign thermistor to fan")
	fmt.Println("      interpolate   true | false")
	fmt.Println("      manual        Set fixed speed (0–100) and enable manual mode")
	fmt.Println("      connected     true | false")
	fmt.Println("      name          Assign a human-readable name")
	fmt.Println("")
	fmt.Println("  set th <ID> <property> <value>")
	fmt.Println("    Properties:")
	fmt.Println("      b_value       Set thermistor B-value")
	fmt.Println("      connected     true | false")
	fmt.Println("      name          Assign a human-readable name")
	fmt.Println("")
	fmt.Println("  list-curves       List all available fan curve names")
	fmt.Println("  list-serial       List available serial devices")
	fmt.Println("  help              Show this help")
	fmt.Println("")
}

func listFanCurves() {
	config, err := fetchConfigFromDaemon()
	if err != nil {
		log.Fatalf("Unable to fetch config: %v", err)
	}
	fmt.Println("Available Fan curves:")
	for name := range config.Curves {
		fmt.Printf(" - %s\n", name)
	}
}

func handleSetCommand() {
	config, err := fetchConfigFromDaemon()
	if err != nil {
		log.Fatalf("Unable to fetch config: %v", err)
	}
	args := os.Args[2:]
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: set <fan|th> <ID> <property> <value>")
		os.Exit(1)
	}

	targetType, rawID, key, value := args[0], args[1], args[2], args[3]

	switch targetType {
	case "fan":
		targetID := "fan" + rawID
		fan, ok := config.Fans[targetID]
		if !ok {
			fmt.Fprintf(os.Stderr, "Fan '%s' not found\n", targetID)
			os.Exit(1)
		}
		switch key {
		case "curve":
			if _, ok := config.Curves[value]; !ok {
				fmt.Fprintf(os.Stderr, "Curve '%s' not found\n", value)
				os.Exit(1)
			}
			fan.Curve = value
			fan.Manual = false
		case "thermistor":
			if _, ok := config.Thermistors[value]; !ok {
				fmt.Fprintf(os.Stderr, "Thermistor '%s' not found\n", value)
				os.Exit(1)
			}
			fan.Thermistor = value
		case "interpolate":
			fan.Interpolation = value == "true"
		case "manual":
			speed, err := strconv.Atoi(value)
			if err != nil || speed < 0 || speed > 100 {
				fmt.Fprintln(os.Stderr, "Manual speed must be 0–100")
				os.Exit(1)
			}
			fan.Manual = true
			fan.FixedSpeed = speed
		case "name":
			fan.Name = value
		case "connected":
			isConnected := value == "true"
			fan.Connected = isConnected
		default:
			fmt.Fprintf(os.Stderr, "Unknown fan property: %s\n", key)
			os.Exit(1)
		}
		config.Fans[targetID] = fan

	case "th":
		targetID := "th" + rawID
		th, ok := config.Thermistors[targetID]
		if !ok {
			fmt.Fprintf(os.Stderr, "Thermistor '%s' not found\n", targetID)
			os.Exit(1)
		}
		switch key {
		case "b_value":
			bv, err := strconv.Atoi(value)
			if err != nil || bv <= 0 {
				fmt.Fprintln(os.Stderr, "Invalid B-value")
				os.Exit(1)
			}
			th.BValue = bv
		case "name":
			th.Name = value
		case "connected":
			isConnected := value == "true"
			th.Connected = isConnected
		default:
			fmt.Fprintf(os.Stderr, "Unknown thermistor property: %s\n", key)
			os.Exit(1)
		}
		config.Thermistors[targetID] = th

	default:
		fmt.Fprintf(os.Stderr, "Unknown target type: %s\n", targetType)
		os.Exit(1)
	}

	if err := validateConfig(config); err != nil {
		fmt.Fprintf(os.Stderr, "Config validation failed: %v\n", err)
		os.Exit(1)
	}
	if err := sendConfigToDaemon(config); err != nil {
		log.Fatalf("Unable to update config: %v", err)
	}

	fmt.Println("Configuration updated.")
}

func listSerialDevices() {
	fmt.Println("Available serial devices:")
	devices := findSerialDevices()
	if len(devices) == 0 {
		fmt.Println("No serial devices found")
		return
	}
	for _, device := range devices {
		fmt.Printf(" - %s\n", device)
	}
}

func findSerialDevices() []string {
	var devices []string
	var searchPaths []string
	switch runtime.GOOS {
	case "darwin":
		searchPaths = []string{
			"/dev/tty.usb*",
		}
	case "linux":
		searchPaths = []string{
			"/dev/ttyUSB*",
			"/dev/ttyACM*",
		}
	case "freebsd":
		searchPaths = []string{
			"/dev/cuaU*",
		}
	default:
		searchPaths = []string{
			"/dev/tty*",
			"/dev/cu*",
		}
	}

	for _, pattern := range searchPaths {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {

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

	var preferred []string
	switch runtime.GOOS {
	case "darwin":
		preferred = []string{"tty.usbmodem"}
	case "linux":
		preferred = []string{"ttyUSB", "ttyACM"}
	case "freebsd":
		preferred = []string{"cuaU"}
	}

	// Sort preferred first
	sorted := sortDevicesByPreference(devices, preferred)

	for _, dev := range sorted {
		port, err := openSerialPort(dev)
		if err != nil {
			continue
		}
		log.Printf("Probing serial device: %s", dev)

		controller := &Controller{serialPort: port}
		err = controller.sendCommand(Command{Cmd: "alive"})
		port.Close()

		if err == nil {
			log.Printf("Found device: %s", dev)
			return dev
		} else {
			log.Printf("Rejected %s: %v", dev, err)
		}
	}
	return ""
}

func openSerialPort(path string) (serial.Port, error) {
	mode := &serial.Mode{
		BaudRate: DefaultBaudRate,
	}
	return serial.Open(path, mode)
}

func sortDevicesByPreference(devices, preferredPrefixes []string) []string {
	var preferred, rest []string
	for _, d := range devices {
		matched := false
		for _, prefix := range preferredPrefixes {
			if strings.Contains(d, prefix) {
				preferred = append(preferred, d)
				matched = true
				break
			}
		}
		if !matched {
			rest = append(rest, d)
		}
	}
	return append(preferred, rest...)
}

func getPlatformSocketPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/tmp/pico-fan-controller.sock"
	case "linux":
		return "/run/pico-fan-controller.sock"
	case "freebsd":
		return "/var/run/pico-fan-controller.sock"
	default:
		return "/tmp/pico-fan-controller.sock"
	}
}

func validateConfig(config *Config) error {
	if config.Server.SocketPath == "" {
		return fmt.Errorf("server.socket_path must not be empty")
	}
	if config.Server.UpdateRate <= 0 {
		return fmt.Errorf("server.update_rate must be greater than 0")
	}
	if config.Server.BaudRate <= 0 {
		return fmt.Errorf("server.baud_rate must be greater than 0")
	}
	if config.Server.LogPath == "" {
		return fmt.Errorf("server.log_path must not be empty")
	}

	for name, therm := range config.Thermistors {
		if therm.BValue <= 0 {
			return fmt.Errorf("thermistor '%s' has invalid B-value: %d", name, therm.BValue)
		}
	}

	for fanName, fanConfig := range config.Fans {
		if fanConfig.Thermistor == "" {
			return fmt.Errorf("fan '%s' has empty thermistor reference", fanName)
		}
		if fanConfig.Manual {
			if fanConfig.FixedSpeed < 0 || fanConfig.FixedSpeed > 100 {
				return fmt.Errorf("fan '%s' has invalid fixed_speed: %d", fanName, fanConfig.FixedSpeed)
			}
		}
		if _, ok := config.Thermistors[fanConfig.Thermistor]; !ok {
			return fmt.Errorf("fan '%s' references non-existent thermistor '%s'", fanName, fanConfig.Thermistor)
		}
		if fanConfig.Curve == "" {
			return fmt.Errorf("fan '%s' has empty curve reference", fanName)
		}
		if _, ok := config.Curves[fanConfig.Curve]; !ok {
			return fmt.Errorf("fan '%s' references non-existent curve '%s'", fanName, fanConfig.Curve)
		}
		if fanConfig.MinSpeed < 0 || fanConfig.MaxSpeed > 100 || fanConfig.MinSpeed > fanConfig.MaxSpeed {
			return fmt.Errorf("fan '%s' has invalid speed range: min=%d max=%d", fanName, fanConfig.MinSpeed, fanConfig.MaxSpeed)
		}
	}

	for curveName, points := range config.Curves {
		if len(points) < 2 {
			return fmt.Errorf("curve '%s' must contain at least 2 points", curveName)
		}
		prevTemp := -1.0
		for i, point := range points {
			if point.Temp < 0 {
				return fmt.Errorf("curve '%s', point %d has negative temperature: %.1f", curveName, i, point.Temp)
			}
			if point.Speed < 0 || point.Speed > 100 {
				return fmt.Errorf("curve '%s', point %d has invalid speed: %d", curveName, i, point.Speed)
			}
			if point.Temp <= prevTemp {
				return fmt.Errorf("curve '%s' points must be in strictly increasing temperature order", curveName)
			}
			prevTemp = point.Temp
		}
	}

	return nil
}

func loadConfig(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		config := createDefaultConfig()

		if err := validateConfig(config); err != nil {
			return nil, fmt.Errorf("default config validation failed: %v", err)
		}
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

	if config.Server.SocketPath == "" {
		config.Server.SocketPath = getPlatformSocketPath()
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
			SocketPath:   getPlatformSocketPath(),
			UpdateRate:   DefaultUpdateRateMs * time.Millisecond,
			SerialDevice: "",
			BaudRate:     DefaultBaudRate,
			AutoDetect:   true,
			LogPath:      "./pico-fan-controller.log",
		},
		Fans: map[string]FanConfig{
			"fan1": {
				Thermistor:    "th1",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
				Manual:        false,
				FixedSpeed:    40,
				Name:          "",
				Connected:     true,
			},
			"fan2": {
				Thermistor:    "th1",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
				Manual:        false,
				FixedSpeed:    40,
				Name:          "",
				Connected:     true,
			},
			"fan3": {
				Thermistor:    "th2",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
				Manual:        false,
				FixedSpeed:    40,
				Name:          "",
				Connected:     true,
			},
			"fan4": {
				Thermistor:    "th2",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
				Manual:        false,
				FixedSpeed:    40,
				Name:          "",
				Connected:     true,
			},
			"fan5": {
				Thermistor:    "th3",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
				Manual:        false,
				FixedSpeed:    40,
				Name:          "",
				Connected:     true,
			},
			"fan6": {
				Thermistor:    "th3",
				Curve:         "default",
				MinSpeed:      DefaultMinFanSpeed,
				MaxSpeed:      DefaultMaxFanSpeed,
				Interpolation: true,
				Manual:        false,
				FixedSpeed:    40,
				Name:          "",
				Connected:     true,
			},
		},
		Thermistors: map[string]ThermConfig{
			"th1": {
				BValue:    DefaultThermBValue,
				Name:      "",
				Connected: true,
			},
			"th2": {
				BValue:    DefaultThermBValue,
				Name:      "",
				Connected: true,
			},
			"th3": {
				BValue:    DefaultThermBValue,
				Name:      "",
				Connected: true,
			},
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

func fetchConfigFromDaemon() (*Config, error) {
	conn, err := connectToSocket()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(Command{Cmd: "get-config"}); err != nil {
		return nil, fmt.Errorf("failed to send get-config: %w", err)
	}

	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	if resp.Status != "ok" {
		return nil, fmt.Errorf("daemon error: %s", resp.Msg)
	}

	// Convert map back to Config
	data, err := json.Marshal(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal error: %w", err)
	}
	return &cfg, nil
}

func sendConfigToDaemon(cfg *Config) error {
	conn, err := connectToSocket()
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(Command{Cmd: "set-config", Data: cfg}); err != nil {
		return fmt.Errorf("failed to send set-config: %w", err)
	}

	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	if resp.Status != "ok" {
		return fmt.Errorf("daemon rejected config: %s", resp.Msg)
	}
	return nil
}

func isDaemonRunning() bool {
	conn, err := net.Dial("unix", getPlatformSocketPath())
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func startController(config *Config, configFile *string) {
	controller := &Controller{
		config:     config,
		configFile: configFile,
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		cancel()
	}()

	if err := controller.connectToDevice(); err != nil {
		log.Fatalf("Failed to connect to device: %v", err)
	}
	defer controller.serialPort.Close()

	if err := controller.configureThermistors(); err != nil {
		log.Fatalf("Failed to configure thermistors: %v", err)
	}

	go controller.startSocketServer(ctx)

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
		thNum, err := strconv.Atoi(thName[2:])
		if err != nil {
			log.Printf("Warning: Invalid thermistor name '%s', skipping configuration", thName)
			continue
		}
		cmd := Command{
			Cmd: "set-th-config",
			Data: SetThConfigPayload{
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

	os.Remove(getPlatformSocketPath())
	listener, err := net.Listen("unix", getPlatformSocketPath())
	if err != nil {
		log.Fatalf("Failed to create Unix socket: %v", err)
	}
	defer listener.Close()
	defer os.Remove(getPlatformSocketPath())
	log.Printf("Socket server started at %s", getPlatformSocketPath())
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
		encoder.Encode(Response{Status: "ok", Data: c.measurements})
	case "get-config":
		encoder.Encode(Response{
			Status: "ok",
			Data:   c.config,
		})
	case "set-config":
		raw, _ := json.Marshal(cmd.Data) // already unmarshaled by decoder
		var newCfg Config
		if err := json.Unmarshal(raw, &newCfg); err != nil {
			encoder.Encode(Response{Status: "error", Msg: "invalid config"})
			return
		}
		if err := validateConfig(&newCfg); err != nil {
			encoder.Encode(Response{Status: "error", Msg: err.Error()})
			return
		}
		c.config = &newCfg
		if err := saveConfig(&newCfg, *c.configFile); err != nil {
			encoder.Encode(Response{Status: "error", Msg: "failed to save config"})
			return
		}
		encoder.Encode(Response{Status: "ok"})
		log.Println("Configuration updated via socket")

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
	for _, fanName := range SortKeys(c.config.Fans) {
		fanConfig := c.config.Fans[fanName]
		thData, exists := c.measurements.Thermometers[fanConfig.Thermistor]
		if !exists {
			log.Printf("Warning: Thermistor '%s' for fan '%s' not found in measurements", fanConfig.Thermistor, fanName)
			continue
		}

		var targetSpeed int
		if fanConfig.Manual {
			targetSpeed = fanConfig.FixedSpeed
		} else {
			targetSpeed = c.calculateSpeedFromCurve(thData.Temp, fanConfig.Curve, fanConfig)
		}

		if targetSpeed < fanConfig.MinSpeed {
			targetSpeed = fanConfig.MinSpeed
		}
		if targetSpeed > fanConfig.MaxSpeed {
			targetSpeed = fanConfig.MaxSpeed
		}

		targetSpeed = (targetSpeed / FanSpeedStepSize) * FanSpeedStepSize

		currentSpeed := 0
		if fanData, exists := c.measurements.Fans[fanName]; exists {
			currentSpeed = fanData.Speed
		}

		if targetSpeed != currentSpeed {
			fanNum, err := strconv.Atoi(fanName[3:])
			if err != nil {
				continue
			}
			cmd := Command{
				Cmd: "set-fan-speed",
				Data: SetFanSpeedPayload{
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

	sort.Slice(curve, func(i, j int) bool {
		return curve[i].Temp < curve[j].Temp
	})

	if temp <= curve[0].Temp {
		return curve[0].Speed
	}

	if temp >= curve[len(curve)-1].Temp {
		return curve[len(curve)-1].Speed
	}

	for i := 0; i < len(curve)-1; i++ {
		if temp >= curve[i].Temp && temp <= curve[i+1].Temp {
			if fanConfig.Interpolation {

				tempRange := curve[i+1].Temp - curve[i].Temp
				speedRange := float64(curve[i+1].Speed - curve[i].Speed)
				tempOffset := temp - curve[i].Temp
				speed := float64(curve[i].Speed) + (tempOffset/tempRange)*speedRange
				return int(speed + 0.5)
			} else {

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
	data = append(data, '\n')

	if _, err := c.serialPort.Write(data); err != nil {
		return err
	}

	// Use context for timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	responseCh := make(chan []byte, 1)
	errCh := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(c.serialPort)
		if !scanner.Scan() {
			errCh <- fmt.Errorf("no response from device")
			return
		}
		responseCh <- scanner.Bytes()
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for device response")
	case err := <-errCh:
		return err
	case data := <-responseCh:
		var response Response
		if err := json.Unmarshal(data, &response); err != nil {
			return fmt.Errorf("invalid response: %v", err)
		}
		if response.Status == "error" {
			return fmt.Errorf("device error: %s", response.Msg)
		}
		if cmd.Cmd == "get-measurements" {
			dataBytes, err := json.Marshal(response.Data)
			if err != nil {
				return fmt.Errorf("failed to encode measurements: %v", err)
			}
			var m Measurements
			if err := json.Unmarshal(dataBytes, &m); err != nil {
				return fmt.Errorf("failed to decode measurements: %v", err)
			}
			c.measurements = m
		}

		return nil
	}
}

func connectToSocket() (net.Conn, error) {
	return net.Dial("unix", getPlatformSocketPath())
}

func maxDisplayWidth[T any](m map[string]T, getName func(string, T) string) int {
	maxLen := 0
	for _, id := range SortKeys(m) {
		name := getName(id, m[id])
		if len(name) > maxLen {
			maxLen = len(name)
		}
	}
	return maxLen
}

func showMeasurements(jsonOutput bool) {
	conn, err := connectToSocket()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(Command{Cmd: "get-measurements"}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to send command: %v\n", err)
		os.Exit(1)
	}

	var response Response
	if err := decoder.Decode(&response); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	if response.Status != "ok" {
		fmt.Fprintf(os.Stderr, "Daemon error: %s\n", response.Msg)
		os.Exit(1)
	}

	dataBytes, err := json.Marshal(response.Data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to encode response data: %v\n", err)
		os.Exit(1)
	}

	var measurements Measurements
	if err := json.Unmarshal(dataBytes, &measurements); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid measurement format: %v\n", err)
		os.Exit(1)
	}

	config, err := fetchConfigFromDaemon()
	if err != nil {
		log.Fatalf("Unable to fetch config: %v", err)
	}

	for fanID, fan := range measurements.Fans {
		cfg, exists := config.Fans[fanID]
		if exists {
			fan.Name = cfg.Name // can be empty
			fan.Connected = cfg.Connected
		} else {
			fan.Connected = false
		}
		measurements.Fans[fanID] = fan
	}

	for thID, th := range measurements.Thermometers {
		cfg, exists := config.Thermistors[thID]
		if exists {
			th.Name = cfg.Name
			th.Connected = cfg.Connected
		} else {
			th.Connected = false
		}
		measurements.Thermometers[thID] = th
	}

	if jsonOutput {
		pretty, err := json.MarshalIndent(measurements, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to encode JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(pretty))
		return
	}

	if measurements.Thermometers != nil {
		fmt.Println("Thermistors:")
		width := maxDisplayWidth(measurements.Thermometers, func(id string, data ThermData) string {
			if data.Name != "" {
				return data.Name
			}
			return id
		})
		for _, id := range SortKeys(measurements.Thermometers) {
			data := measurements.Thermometers[id]
			if !data.Connected {
				continue // skip disconnected
			}
			display := id
			if data.Name != "" {
				display = data.Name
			}
			fmt.Printf("  %-*s  %.1f°C (B-value: %d)\n", width, display, data.Temp, data.BValue)
		}
		fmt.Println()
	}

	if measurements.Fans != nil {
		fmt.Println("Fans:")
		width := maxDisplayWidth(measurements.Fans, func(id string, data FanData) string {
			if data.Name != "" {
				return data.Name
			}
			return id
		})
		for _, id := range SortKeys(measurements.Fans) {
			data := measurements.Fans[id]
			if !data.Connected {
				continue // skip disconnected
			}
			display := id
			if data.Name != "" {
				display = data.Name
			}
			fmt.Printf("  %-*s  %3d%% (%d RPM)\n", width, display, data.Speed, data.RPM)
		}
	}
}

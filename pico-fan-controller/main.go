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

	"github.com/gosnmp/gosnmp"
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
	DefaultSNMPPort     = 161
	DefaultSNMPInterval = 30
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
	Status string       `json:"status,omitempty"`
	Msg    string       `json:"msg,omitempty"`
	Data   Measurements `json:"data,omitempty"`
}

type Measurements struct {
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
	SNMP         SNMPConfig    `yaml:"snmp"`
	LogPath      string        `yaml:"log_path"`
}

type SNMPConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Host        string        `yaml:"host"`
	Port        uint16        `yaml:"port"`
	Community   string        `yaml:"community"`
	Interval    time.Duration `yaml:"interval"`
	TempOIDBase string        `yaml:"temp_oid_base"`
	FanOIDBase  string        `yaml:"fan_oid_base"`
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

type Controller struct {
	config       *Config
	serialPort   serial.Port
	measurements Measurements
	detached     bool
	snmpClient   *gosnmp.GoSNMP
}

func SortKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func startAsDaemon(configPath string, logFile string) {
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}

	cmd := exec.Command(os.Args[0], "run", "--config", configPath)
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
		startController(config)

	case "show":
		showCmd := flag.NewFlagSet("show", flag.ExitOnError)
		jsonOutput := showCmd.Bool("json", false, "Output measurements as JSON")
		showCmd.Parse(os.Args[2:])

		if !isDaemonRunning() {
			fmt.Println("Daemon is not running")
			os.Exit(1)
		}
		showMeasurements(*jsonOutput)

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
	fmt.Println("  list-serial       List available serial devices")
	fmt.Println("  help              Show this help")
	fmt.Println("")
}

func (c *Controller) initSNMP() error {
	if !c.config.Server.SNMP.Enabled {
		return nil
	}
	c.snmpClient = &gosnmp.GoSNMP{
		Target:    c.config.Server.SNMP.Host,
		Port:      c.config.Server.SNMP.Port,
		Community: c.config.Server.SNMP.Community,
		Version:   gosnmp.Version2c,
		Timeout:   time.Duration(2) * time.Second,
	}
	if err := c.snmpClient.Connect(); err != nil {
		return fmt.Errorf("failed to connect to SNMP server: %v", err)
	}
	log.Printf("SNMP client connected to %s:%d", c.config.Server.SNMP.Host, c.config.Server.SNMP.Port)
	return nil
}

func (c *Controller) sendSNMPData() error {
	if !c.config.Server.SNMP.Enabled || c.snmpClient == nil {
		return nil
	}
	var pdus []gosnmp.SnmpPDU

	for name, data := range c.measurements.Thermometers {
		oid := fmt.Sprintf("%s.%s", c.config.Server.SNMP.TempOIDBase, name)
		pdus = append(pdus, gosnmp.SnmpPDU{
			Name:  oid,
			Type:  gosnmp.OctetString,
			Value: fmt.Sprintf("%.1f", data.Temp),
		})
	}

	for name, data := range c.measurements.Fans {
		speedOID := fmt.Sprintf("%s.%s.speed", c.config.Server.SNMP.FanOIDBase, name)
		rpmOID := fmt.Sprintf("%s.%s.rpm", c.config.Server.SNMP.FanOIDBase, name)
		pdus = append(pdus,
			gosnmp.SnmpPDU{
				Name:  speedOID,
				Type:  gosnmp.Integer,
				Value: data.Speed,
			},
			gosnmp.SnmpPDU{
				Name:  rpmOID,
				Type:  gosnmp.Integer,
				Value: data.RPM,
			},
		)
	}
	if len(pdus) > 0 {
		_, err := c.snmpClient.Set(pdus)
		if err != nil {
			return fmt.Errorf("failed to send SNMP data: %v", err)
		}
	}
	return nil
}

func (c *Controller) startSNMPSender(ctx context.Context) {
	if !c.config.Server.SNMP.Enabled {
		return
	}
	ticker := time.NewTicker(c.config.Server.SNMP.Interval * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.sendSNMPData(); err != nil {
				log.Printf("SNMP send error: %v", err)
			}
		}
	}
}

func listSerialDevices() {
	fmt.Println("Available serial devices:")
	fmt.Println("=========================")
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

	snmp := config.Server.SNMP
	if snmp.Enabled {
		if snmp.Host == "" {
			return fmt.Errorf("snmp.host must not be empty")
		}
		if !(snmp.Port > 1 && snmp.Port < 65535) {
			return fmt.Errorf("snmp.port must be between 1 and 65535")
		}
		if snmp.Community == "" {
			return fmt.Errorf("snmp.community must not be empty")
		}
		if snmp.Interval <= 0 {
			return fmt.Errorf("snmp.interval must be greater than 0")
		}
		if snmp.TempOIDBase == "" || snmp.FanOIDBase == "" {
			return fmt.Errorf("snmp.oid_base values must not be empty")
		}
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
			SNMP: SNMPConfig{
				Enabled:     false,
				Host:        "localhost",
				Port:        DefaultSNMPPort,
				Community:   "public",
				Interval:    DefaultSNMPInterval,
				TempOIDBase: "1.3.6.1.4.1.99999.1.1",
				FanOIDBase:  "1.3.6.1.4.1.99999.1.2",
			},
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
	conn, err := net.Dial("unix", getPlatformSocketPath())
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func startController(config *Config) {
	controller := &Controller{
		config: config,
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

	if err := controller.initSNMP(); err != nil {
		log.Printf("SNMP initialization failed: %v", err)
	}
	defer func() {
		if controller.snmpClient != nil {
			controller.snmpClient.Conn.Close()
		}
	}()

	go controller.startSocketServer(ctx)

	go controller.startSNMPSender(ctx)

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

		targetSpeed := c.calculateSpeedFromCurve(thData.Temp, fanConfig.Curve, fanConfig)

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
			c.measurements = response.Data
		}
		return nil
	}
}

func connectToSocket() (net.Conn, error) {
	return net.Dial("unix", getPlatformSocketPath())
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
	cmd := Command{Cmd: "get-measurements"}

	if err := encoder.Encode(cmd); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to send command: %v\n", err)
		os.Exit(1)
	}

	var response Response
	if err := decoder.Decode(&response); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	if jsonOutput {
		pretty, err := json.MarshalIndent(response, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to encode JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(pretty))
		return
	}

	fmt.Println("Pico Fan Controller Measurements")
	fmt.Println("================================")
	fmt.Println()
	if response.Data.Thermometers != nil {
		fmt.Println("Thermistors:")
		for _, name := range SortKeys(response.Data.Thermometers) {
			data := response.Data.Thermometers[name]
			fmt.Printf("  %s: %.1f°C (B-value: %d)\n", name, data.Temp, data.BValue)
		}
		fmt.Println()
	}

	if response.Data.Fans != nil {
		fmt.Println("Fans:")
		for _, name := range SortKeys(response.Data.Fans) {
			data := response.Data.Fans[name]
			fmt.Printf("  %s: %d%% (%d RPM)\n", name, data.Speed, data.RPM)
		}
	}
}

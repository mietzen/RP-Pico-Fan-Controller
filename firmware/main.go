package main

import (
	"encoding/json"
	"machine"
	"math"
	"sync/atomic"
	"time"
)

const (
	DefaultBValue    = 3450
	InvalidTemp      = -999.9
	DefaultFanSpeed  = 60      // Percent
	PwmFrequency     = 25000   // Hz
	ResistorValue    = 10000.0 // Ohm
	ThermistorValue  = 10000.0 // Ohm
	WatchdogInterval = 1000    // milliseconds
	WatchdogTimeout  = 10000   // milliseconds
	StartupDelay     = 2000    // milliseconds
)

type Fan struct {
	pwmPin     machine.Pin
	rpmPin     machine.Pin
	pwm        PWM
	channel    uint8
	speed      uint32
	rpm        uint32
	pulseCount uint64
}

type Thermistor struct {
	adc    machine.ADC
	bValue float64
	temp   float64
}

type PWM interface {
	Top() uint32
	Set(ch uint8, value uint32)
	Channel(pin machine.Pin) (uint8, error)
	Configure(machine.PWMConfig) error
}

type Command struct {
	Cmd  string          `json:"cmd"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Response struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
	Msg    string          `json:"msg,omitempty"`
}

type SetThConfig struct {
	Th     int `json:"th"`
	BValue int `json:"b-value"`
}

type SetFanSpeed struct {
	Fan   int `json:"fan"`
	Speed int `json:"speed"`
}

type FanData struct {
	Speed int `json:"speed"`
	RPM   int `json:"rpm"`
}

type ThermData struct {
	Temp   float64 `json:"temp"`
	BValue int     `json:"b-value"`
}

type Measurements struct {
	Fans         map[string]FanData   `json:"fans"`
	Thermometers map[string]ThermData `json:"thermometers"`
}

var (
	fans           [6]Fan
	thermistors    [3]Thermistor
	serialBuffer   []byte
	lastAliveTime  time.Time
	watchdogActive bool
	led            machine.Pin
)

func main() {
	time.Sleep(StartupDelay * time.Millisecond)
	println("Fan Controller Starting...")

	led = machine.LED
	led.Configure(machine.PinConfig{Mode: machine.PinOutput})
	led.High()

	initializeFans()
	initializeThermistors()
	startRPMCalculation()
	startTempReading()
	setAllFansToDefaultSpeed()
	startAliveWatchdog()

	lastAliveTime = time.Now()
	watchdogActive = true

	println("Ready for commands")

	for {
		processSerial()
		time.Sleep(time.Millisecond * 10)
	}
}

func initializeFans() {
	fanConfigs := []struct {
		rpmPin machine.Pin
		pwmPin machine.Pin
		pwm    PWM
	}{
		{machine.GP2, machine.GP3, machine.PWM1},
		{machine.GP4, machine.GP5, machine.PWM2},
		{machine.GP6, machine.GP7, machine.PWM3},
		{machine.GP8, machine.GP9, machine.PWM4},
		{machine.GP10, machine.GP11, machine.PWM5},
		{machine.GP12, machine.GP13, machine.PWM6},
	}

	for i := range fans {
		fans[i].pwmPin = fanConfigs[i].pwmPin
		fans[i].rpmPin = fanConfigs[i].rpmPin
		fans[i].pwm = fanConfigs[i].pwm
		fans[i].speed = 0
		fans[i].pulseCount = 0

		err := fans[i].pwm.Configure(machine.PWMConfig{
			Period: uint64(1e9 / PwmFrequency),
		})
		if err != nil {
			println("PWM config error fan", i+1, ":", err.Error())
			continue
		}

		ch, err := fans[i].pwm.Channel(fans[i].pwmPin)
		if err != nil {
			println("PWM channel error fan", i+1, ":", err.Error())
			continue
		}
		fans[i].channel = ch

		fans[i].rpmPin.Configure(machine.PinConfig{Mode: machine.PinInputPullup})

		fanIndex := i
		err = fans[i].rpmPin.SetInterrupt(machine.PinFalling, func(p machine.Pin) {
			atomic.AddUint64(&fans[fanIndex].pulseCount, 1)
		})
		if err != nil {
			println("Interrupt error fan", i+1, ":", err.Error())
		}
	}
}

func initializeThermistors() {
	adcPins := []machine.Pin{machine.ADC0, machine.ADC1, machine.ADC2}

	machine.InitADC()

	for i := range thermistors {
		thermistors[i].adc = machine.ADC{Pin: adcPins[i]}
		thermistors[i].adc.Configure(machine.ADCConfig{})
		thermistors[i].bValue = DefaultBValue
	}
}

func startRPMCalculation() {
	go func() {
		ticker := time.NewTicker(time.Second)
		for range ticker.C {
			for i := range fans {
				count := atomic.SwapUint64(&fans[i].pulseCount, 0)
				// 2 pulses per revolution
				fans[i].rpm = uint32((count / 2) * 60)
			}
		}
	}()
}

func startTempReading() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		for range ticker.C {
			for i := range thermistors {
				readTemperature(i)
			}
		}
	}()
}

func startAliveWatchdog() {
	go func() {
		ticker := time.NewTicker(WatchdogInterval * time.Millisecond)
		for range ticker.C {
			if watchdogActive && time.Since(lastAliveTime) > WatchdogTimeout*time.Millisecond {
				println("Alive timeout - setting all fans to 60%")
				setAllFansToDefaultSpeed()
				watchdogActive = false
				go blinkLED()
			}
		}
	}()
}

func setAllFansToDefaultSpeed() {
	for i := range fans {
		setFanSpeed(i, DefaultFanSpeed)
	}
}

func blinkLED() {
	for !watchdogActive {
		led.Low()
		time.Sleep(time.Millisecond * 500)
		led.High()
		time.Sleep(time.Millisecond * 500)
	}
	// Keep LED solid on when watchdog is active again
	led.High()
}

func readTemperature(index int) {
	adcValue := float64(thermistors[index].adc.Get())

	if adcValue < 1 {
		println("Warning: ADC read 0 on thermistor", index+1)
		thermistors[index].temp = InvalidTemp
		return
	}

	// 16-bit ADC
	r := ResistorValue * (65535.0/adcValue - 1.0)
	if r <= 0 {
		println("Warning: invalid resistance on thermistor", index+1)
		thermistors[index].temp = InvalidTemp
		return
	}

	lnr := math.Log(r / ThermistorValue)

	// Steinhart-Hart
	invT0 := 1.0 / 298.15 // 25C in Kelvin
	invT := invT0 + lnr/thermistors[index].bValue
	thermistors[index].temp = math.Round(((1.0/invT)-273.15)*10.0) / 10.0
}

func setFanSpeed(fanIndex int, speed int) error {
	if fanIndex < 0 || fanIndex >= len(fans) {
		return nil
	}
	if speed < 0 || speed > 100 {
		return nil
	}

	fans[fanIndex].speed = uint32(speed)
	duty := fans[fanIndex].pwm.Top() * uint32(speed) / 100
	fans[fanIndex].pwm.Set(fans[fanIndex].channel, duty)
	return nil
}

func processSerial() {
	c, err := machine.Serial.ReadByte()
	if err != nil {
		return
	}

	if c == '\n' || c == '\r' {
		if len(serialBuffer) > 512 {
			serialBuffer = serialBuffer[:0]
			println("Serial buffer overflow, reset")
		}
		if len(serialBuffer) > 0 {
			processCommand(string(serialBuffer))
			serialBuffer = serialBuffer[:0]
		}
		return
	}

	serialBuffer = append(serialBuffer, c)
}

func processCommand(cmdStr string) {
	var cmd Command
	err := json.Unmarshal([]byte(cmdStr), &cmd)
	if err != nil {
		sendResponse(Response{Status: "error", Msg: "Invalid JSON"})
		return
	}

	switch cmd.Cmd {
	case "alive":
		lastAliveTime = time.Now()
		watchdogActive = true
		led.High()
		sendResponse(Response{Status: "ok"})

	case "set-th-config":
		var config SetThConfig
		err := json.Unmarshal(cmd.Data, &config)
		if err != nil {
			sendResponse(Response{Status: "error", Msg: "Invalid data"})
			return
		}

		if config.Th < 1 || config.Th > len(thermistors) {
			sendResponse(Response{Status: "error", Msg: "Invalid thermistor number"})
			return
		}

		thermistors[config.Th-1].bValue = float64(config.BValue)
		sendResponse(Response{Status: "ok"})

	case "set-fan-speed":
		var speed SetFanSpeed
		err := json.Unmarshal(cmd.Data, &speed)
		if err != nil {
			sendResponse(Response{Status: "error", Msg: "Invalid data"})
			return
		}

		if speed.Fan < 1 || speed.Fan > len(fans) {
			sendResponse(Response{Status: "error", Msg: "Invalid fan number"})
			return
		}

		if speed.Speed < 0 || speed.Speed > 100 {
			sendResponse(Response{Status: "error", Msg: "Invalid speed"})
			return
		}

		err = setFanSpeed(speed.Fan-1, speed.Speed)
		if err != nil {
			sendResponse(Response{Status: "error", Msg: "Failed to set speed"})
		} else {
			sendResponse(Response{Status: "ok"})
		}

	case "get-measurements":
		measurements := Measurements{
			Fans: map[string]FanData{
				"fan1": {Speed: int(fans[0].speed), RPM: int(fans[0].rpm)},
				"fan2": {Speed: int(fans[1].speed), RPM: int(fans[1].rpm)},
				"fan3": {Speed: int(fans[2].speed), RPM: int(fans[2].rpm)},
				"fan4": {Speed: int(fans[3].speed), RPM: int(fans[3].rpm)},
				"fan5": {Speed: int(fans[4].speed), RPM: int(fans[4].rpm)},
				"fan6": {Speed: int(fans[5].speed), RPM: int(fans[5].rpm)},
			},
			Thermometers: map[string]ThermData{
				"th1": {Temp: thermistors[0].temp, BValue: int(thermistors[0].bValue)},
				"th2": {Temp: thermistors[1].temp, BValue: int(thermistors[1].bValue)},
				"th3": {Temp: thermistors[2].temp, BValue: int(thermistors[2].bValue)},
			},
		}

		data, err := json.Marshal(measurements)
		if err != nil {
			sendResponse(Response{Status: "error", Msg: "Failed to encode response"})
		} else {
			sendResponse(Response{Status: "ok", Data: data})
		}

	default:
		sendResponse(Response{Status: "error", Msg: "Unknown command"})
	}
}

func sendResponse(resp Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		println("Failed to encode response")
		return
	}
	machine.Serial.Write(data)
	machine.Serial.WriteByte('\n')
}

# WIP: Raspberry Pi Pico Fan Controller

This project aims to create a fan controller based on an RP2040 / RP2350 that can be hand-soldered by anyone and will run on any machine!
It uses only the bare minimum of through-hole solder parts to give you:

* 3 thermal sensors
* 6 controllable 12V PWM fans

The idea came up when I was building my home server with consumer parts and realized again that most onboard fan controllers don't work properly on Linux.

The project consists of three parts:

* The TinyGo RP Pico firmware, found in [firmware](https://github.com/mietzen/RP-Pico-Fan-Controller/tree/main/firmware)
* The Golang fan controller, found in [fancontroller](https://github.com/mietzen/RP-Pico-Fan-Controller/tree/main/fancontroller)
* The KiCad PCB design, found in [kicad](https://github.com/mietzen/RP-Pico-Fan-Controller/tree/main/kicad)

## Hardware Prototype

Here are some pictures from my prototype PCB that I’m using for development:

<img width="400" alt="image" src="https://github.com/user-attachments/assets/ada9bada-7a03-466c-8260-3c0725e48bba" />
<img width="400" alt="image" src="https://github.com/user-attachments/assets/7ac83009-69a1-49cc-ba79-a0bf38ed9e85" />
<img width="800" alt="image" src="https://github.com/user-attachments/assets/3fe91008-0d63-4743-8e50-57a4c07f6b14" />

## TODOs

The project is still in development, I still have a couple of TODOs.

* [Incomplete documentation](https://github.com/mietzen/RP-Pico-Fan-Controller/issues/1)
* [Integrate lm-sensors readings](https://github.com/mietzen/RP-Pico-Fan-Controller/issues/2)
* [Emulate as hwmon device on Linux](https://github.com/mietzen/RP-Pico-Fan-Controller/issues/3)
* [Auto-detect which fans / sensors are connected](https://github.com/mietzen/RP-Pico-Fan-Controller/issues/4)
* [Show fans / sensors as disconnected in fan-controller](https://github.com/mietzen/RP-Pico-Fan-Controller/issues/5)
* [New PCB revision with rear-facing USB port](https://github.com/mietzen/RP-Pico-Fan-Controller/issues/6)
* [Create BOM](https://github.com/mietzen/RP-Pico-Fan-Controller/issues/7)

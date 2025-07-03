```json
    {"cmd": "set-th-config","payload": {"th": [1,2,3], "b-value": "<INT>"}} -> {"status": ["ok", "error"], "msg": ""}
    {"cmd": "set-fan-speed","payload": {"fan": [1,2,3,4,5,6], "speed": "<INT>"}} -> {"status": ["ok", "error"], "msg": ""}
    {"cmd": "get-measurements"} -> {
        "fans": {
            "fan1": {"speed": 0-100, "rpm": "<INT>"},
            "fan2": {"speed": 0-100, "rpm": "<INT>"},
            "fan3": {"speed": 0-100, "rpm": "<INT>"},
            "fan4": {"speed": 0-100, "rpm": "<INT>"},
            "fan5": {"speed": 0-100, "rpm": "<INT>"},
            "fan6": {"speed": 0-100, "rpm": "<INT>"},
        },
        "thermometers": {
            "th1": {"temp": "<INT>", "b-value": "<INT>"},
            "th2": {"temp": "<INT>", "b-value": "<INT>"},
            "th3": {"temp": "<INT>", "b-value": "<INT>"}
        }
    }
```
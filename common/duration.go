package common

import (
	"fmt"
	"time"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var stringValue string
	if err := unmarshal(&stringValue); err == nil {
		duration, err := time.ParseDuration(stringValue)
		if err != nil {
			return fmt.Errorf("invalid interval/duration format: %v", err)
		}
		*d = Duration(duration)
		return nil
	}
	var intValue int64
	if err := unmarshal(&intValue); err == nil {
		*d = Duration(time.Duration(intValue) * time.Millisecond)
		return nil
	}
	var floatValue float64
	if err := unmarshal(&floatValue); err == nil {
		*d = Duration(time.Duration(floatValue) * time.Millisecond)
		return nil
	}

	return fmt.Errorf("cannot unmarshal duration value")
}

func (d Duration) Ptr() *Duration {
	return &d
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

func (d Duration) DurationPtr() *time.Duration {
	return (*time.Duration)(&d)
}

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return SonicCfg.Marshal(time.Duration(d).String())
}

// WithDefault returns the duration value if it's positive, otherwise returns the default value.
// This simplifies the common pattern of applying defaults to optional duration configs.
func (d Duration) WithDefault(defaultVal time.Duration) time.Duration {
	if d > 0 {
		return time.Duration(d)
	}
	return defaultVal
}

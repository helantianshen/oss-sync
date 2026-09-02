package config

import "fmt"

const (
	defaultWebSessionTTLHours = 24
	defaultDeviceJWTTTLHours  = 30 * 24
)

func (c AuthConfig) EffectiveWebSessionTTLHours() int {
	if c.WebSessionTTLHours == 0 {
		return defaultWebSessionTTLHours
	}
	return c.WebSessionTTLHours
}

func (c AuthConfig) EffectiveDeviceJWTTTLHours() int {
	if c.DeviceJWTTTLHours == 0 {
		return defaultDeviceJWTTTLHours
	}
	return c.DeviceJWTTTLHours
}

func (c AuthConfig) validate() error {
	if c.WebSessionTTLHours < 0 {
		return fmt.Errorf("auth.web_session_ttl_hours 不能为负数，收到 %d", c.WebSessionTTLHours)
	}
	if c.DeviceJWTTTLHours < 0 {
		return fmt.Errorf("auth.device_jwt_ttl_hours 不能为负数，收到 %d", c.DeviceJWTTTLHours)
	}
	return nil
}

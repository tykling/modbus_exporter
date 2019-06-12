package modbus

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/lupoDharkael/modbus_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
)

func TestRegisterMetrics(t *testing.T) {
	t.Run("does not fail", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		moduleName := "my_module"
		metrics := []metric{}

		if err := registerMetrics(reg, moduleName, metrics); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("registers metrics with same name and same label keys", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		moduleName := "my_module"
		metrics := []metric{
			{
				Name: "my_metric",
				Help: "my_help",
				Labels: map[string]string{
					"labelKey1": "labelValueA",
					"labelKey2": "labelValueA",
				},
				Value:      1,
				MetricType: config.MetricTypeCounter,
			},
			{
				Name: "my_metric",
				Help: "my_help",
				Labels: map[string]string{
					"labelKey1": "labelValueB",
					"labelKey2": "labelValueB",
				},
				Value:      2,
				MetricType: config.MetricTypeCounter,
			},
		}

		if err := registerMetrics(reg, moduleName, metrics); err != nil {
			t.Fatal(err)
		}

		metricFamilies, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}

		if len(metricFamilies) != 1 {
			t.Fatalf("expected %v but got %v", 1, len(metricFamilies))
		}

		for _, m := range metricFamilies {
			metrics := m.Metric
			if len(metrics) != 2 {
				t.Fatalf("expected %v metrics but got %v", 2, len(metrics))
			}
		}
	})
}

func TestGetModbusData(t *testing.T) {
	tests := []struct {
		name         string
		registers    []config.MetricDef
		registerData func() []byte
		registerType config.RegType
		expect       []float64
	}{
		{
			name: "basic analog input, single register",
			registers: []config.MetricDef{
				{
					Name:     "xyz",
					Address:  22,
					DataType: "uint16",
				},
			},
			registerData: func() []byte {
				// The entire modbus space of digital/ananlog input/output, with
				// 1000 registers, each register spanning two bytes.
				b := make([]byte, 1000*2)

				// Insert register 2.
				insertUInt16(b, 22, uint16(240))

				return b
			},
			registerType: config.AnalogInput,
			expect:       []float64{240},
		},

		{
			name: "analog output, double register, more than 125 (max. analog return length) apart",
			registers: []config.MetricDef{
				{
					Name:     "xyz",
					Address:  2,
					DataType: "uint16",
				},
				{
					Name:     "xyz",
					Address:  299,
					DataType: "uint16",
				},
			},
			registerData: func() []byte {
				// The entire modbus space of digital/ananlog input/output, with
				// 1000 registers, each register spanning two bytes.
				b := make([]byte, 1000*2)

				// Insert register 2.
				insertUInt16(b, 2, uint16(2))

				// Insert register 299.
				insertUInt16(b, 299, uint16(299))

				return b
			},
			registerType: config.AnalogOutput,
			expect:       []float64{2, 299},
		},
	}

	for _, loopTest := range tests {
		test := loopTest

		t.Run(test.name, func(t *testing.T) {
			metrics, err := scrapeModule(
				test.registers,
				func(address, quantity uint16) ([]byte, error) {
					// `(register-1)`: byte slice is zero indexed, registers are not.
					return test.registerData()[(address-1)*2 : (address-1)*2+quantity*2], nil
				},
				test.registerType,
			)

			if err != nil {
				t.Fatal(err)
			}

			if len(metrics) != len(test.expect) {
				t.Fatalf("expected %v floats, but got %v floats back", len(test.expect), len(metrics))
			}

			for i, m := range metrics {
				if m.Value != test.expect[i] {
					t.Fatalf("expected %v but got %v at index %v", test.expect[i], m.Value, i)
				}
			}
		})
	}
}

func insertUInt16(b []byte, register int, v uint16) {
	temp := make([]byte, 2)

	binary.BigEndian.PutUint16(temp, v)

	// Each register spans two bytes.
	// `(register-1)`: byte slice is zero indexed, registers are not.
	b[(register-1)*2] = temp[0]
	b[(register-1)*2+1] = temp[1]
}

func TestParseModbusData(t *testing.T) {
	offsetZero := 0
	offsetOne := 1

	tests := []struct {
		name          string
		input         func() []byte
		metricDef     func() *config.MetricDef
		expectedValue float64
	}{
		{
			name: "bool, no bit",
			input: func() []byte {
				return []byte{uint8(0), uint8(0)}
			},
			metricDef: func() *config.MetricDef {
				return &config.MetricDef{
					DataType:  config.ModbusBool,
					BitOffset: &offsetZero,
				}
			},
			expectedValue: 0,
		},
		{
			name: "bool, first bit",
			input: func() []byte {
				return []byte{uint8(0), uint8(1)}
			},
			metricDef: func() *config.MetricDef {
				return &config.MetricDef{
					DataType:  config.ModbusBool,
					BitOffset: &offsetZero,
				}
			},
			expectedValue: 1,
		},
		{
			name: "bool, second bit",
			input: func() []byte {
				return []byte{uint8(0), uint8(2)}
			},
			metricDef: func() *config.MetricDef {
				return &config.MetricDef{
					DataType:  config.ModbusBool,
					BitOffset: &offsetOne,
				}
			},
			expectedValue: 1,
		},
	}

	for _, loopTest := range tests {

		test := loopTest

		t.Run(test.name, func(t *testing.T) {
			f, err := parseModbusData(*test.metricDef(), test.input())
			if err != nil {
				t.Fatal(err)
			}

			if f != test.expectedValue {
				t.Fatalf("expected metric value to be %v but got %v", test.expectedValue, f)
			}
		})
	}
}

func TestParseModbusDataInsufficientRegisters(t *testing.T) {
	d := config.MetricDef{
		DataType: config.ModbusInt16,
	}

	_, err := parseModbusData(d, []byte{})

	if err == nil {
		t.Fatal("expected error but got nil")
	}

	switch err.(type) {
	case *InsufficientRegistersError:
	default:
		t.Fatal("expected InsufficientRegistersError")
	}
}

func TestParseModbusDataFloat32(t *testing.T) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, math.Float32bits(32))

	def := config.MetricDef{
		DataType: config.ModbusFloat32,
	}

	floatValue, err := parseModbusData(def, data)
	if err != nil {
		t.Fatal(err)
	}

	if floatValue != 32 {
		t.Fatalf("expected 32 but got %v", floatValue)
	}
}

// TestRegisterMetricTwoMetricsSameName makes sure registerMetrics reuses a
// registered metric in case there is a second one with the same name instead of
// reregistering which would cause an exception.
func TestRegisterMetricTwoMetricsSameName(t *testing.T) {
	reg := prometheus.NewRegistry()
	a := metric{"my_metric", "", map[string]string{}, 1, config.MetricTypeCounter}
	b := metric{"my_metric", "", map[string]string{}, 1, config.MetricTypeCounter}

	err := registerMetrics(reg, "my_module", []metric{a, b})
	if err != nil {
		t.Fatalf("expected no error but got: %v", err)
	}
}

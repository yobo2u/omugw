package gateway

import (
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/openairealtime"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	oaws "github.com/yobo2u/omugw/internal/provider/openairealtime"
)

func NewOpenAIRealtimeHandler(d WSDeps) *WSHandler {
	return &WSHandler{d: d, profile: openAIRealtimeProfile()}
}

func openAIRealtimeProfile() wsProfile {
	return wsProfile{
		inbound:         degrade.Inbound{Protocol: degrade.ProtoOpenAIRealtime, Endpoint: degrade.EndpointOpenAIRealtime},
		outbound:        degrade.ProviderOpenAIRealtime,
		validateHeaders: oaws.ValidateHeaders,
		checkReady: func(payload []byte) error {
			if openairealtime.ValidateReady(payload) == nil {
				return nil
			}
			if e, err := openairealtime.Inspect(payload); err == nil && e.Failure != nil {
				return e.Failure
			}
			return errWSRelayPolicy
		},
		encodeError: openaiwire.EncodeError,
		newObserver: func(m *obs.Metrics, inbound, outbound string) wsEventObserver {
			return &openAIWSObserver{usage: newWSUsage(m, inbound, outbound)}
		},
		classifyClose: openairealtime.ClassifyClose,
	}
}

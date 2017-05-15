package hecommplatform

import (
	"github.com/brocaar/lorawan"
)

//ConvertHecommDevEUIToPlatformDevEUI Converts the deveui to platform's specific identifier
func ConvertHecommDevEUIToPlatformDevEUI(devEUI []byte) lorawan.EUI64 {
	var eui lorawan.EUI64
	copy(eui[:], devEUI[0:6])
	return eui
}

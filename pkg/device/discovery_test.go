package device

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSysFSFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// HIGH-9 & MEDIUM-7: discoverFromSysFS reads bConfigurationValue, parses hex bInterfaceNumber (including 00 and 08),
// and records BusType ("usb" vs "pcie").
func TestHigh9AndMedium7DiscoverFromSysFSConfigValueInterfaceNumberAndBusType(t *testing.T) {
	t.Run("USB configuration 2 with hex bInterfaceNumber 08", func(t *testing.T) {
		root := t.TempDir()
		usbDev := filepath.Join(root, "usb1", "1-1")
		writeSysFSFile(t, filepath.Join(usbDev, "idVendor"), "2c7c\n")
		writeSysFSFile(t, filepath.Join(usbDev, "idProduct"), "0800\n")
		writeSysFSFile(t, filepath.Join(usbDev, "bNumInterfaces"), "5\n")
		writeSysFSFile(t, filepath.Join(usbDev, "bConfigurationValue"), "2\n")

		// Interface index 4 under configuration 2 -> 1-1:2.4, with USB descriptor bInterfaceNumber = 0x08!
		if4 := filepath.Join(usbDev, "1-1:2.4")
		writeSysFSFile(t, filepath.Join(if4, "bInterfaceNumber"), "08\n")
		if err := os.MkdirAll(filepath.Join(if4, "net", "wwan0"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(if4, "usbmisc", "cdc-wdm0"), 0755); err != nil {
			t.Fatal(err)
		}
		fakeDrv := filepath.Join(root, "drivers", "qmi_wwan")
		if err := os.MkdirAll(fakeDrv, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fakeDrv, filepath.Join(if4, "driver")); err != nil {
			t.Fatal(err)
		}

		// Audio card under configuration 2 -> 1-1:2.5/sound/card3
		if err := os.MkdirAll(filepath.Join(usbDev, "1-1:2.5", "sound", "card3"), 0755); err != nil {
			t.Fatal(err)
		}

		md, err := discoverFromSysFS(usbDev)
		if err != nil {
			t.Fatalf("discoverFromSysFS() error = %v", err)
		}
		if md.NetInterface != "wwan0" || md.ControlPath != "/dev/cdc-wdm0" {
			t.Fatalf("unexpected NetInterface/ControlPath: %+v", md)
		}
		if !md.HasInterfaceNumber || md.InterfaceNumber != 8 {
			t.Fatalf("InterfaceNumber = (%d, %v), want (8, true)", md.InterfaceNumber, md.HasInterfaceNumber)
		}
		if md.BusType != "usb" {
			t.Fatalf("BusType = %q, want \"usb\"", md.BusType)
		}
		if md.AudioDevice != "hw:3,0" || md.AudioCardNum != 3 {
			t.Fatalf("AudioDevice = (%q, %d), want (\"hw:3,0\", 3)", md.AudioDevice, md.AudioCardNum)
		}
	})

	t.Run("Interface 0 (bInterfaceNumber 00) and MHI/PCIe driver detection", func(t *testing.T) {
		root := t.TempDir()
		mhiDev := filepath.Join(root, "mhi0", "0306_00.01.00")
		writeSysFSFile(t, filepath.Join(mhiDev, "idVendor"), "2c7c\n")
		writeSysFSFile(t, filepath.Join(mhiDev, "idProduct"), "0801\n")
		writeSysFSFile(t, filepath.Join(mhiDev, "bNumInterfaces"), "1\n")

		if0 := filepath.Join(mhiDev, "0306_00.01.00:1.0")
		writeSysFSFile(t, filepath.Join(if0, "bInterfaceNumber"), "00\n")
		if err := os.MkdirAll(filepath.Join(if0, "net", "rmnet_mhi0"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(if0, "usbmisc", "cdc-wdm1"), 0755); err != nil {
			t.Fatal(err)
		}
		fakeDrv := filepath.Join(root, "drivers", "mhi_net")
		if err := os.MkdirAll(fakeDrv, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fakeDrv, filepath.Join(if0, "driver")); err != nil {
			t.Fatal(err)
		}

		md, err := discoverFromSysFS(mhiDev)
		if err != nil {
			t.Fatalf("discoverFromSysFS(mhi) error = %v", err)
		}
		if !md.HasInterfaceNumber || md.InterfaceNumber != 0 {
			t.Fatalf("InterfaceNumber = (%d, %v), want (0, true)", md.InterfaceNumber, md.HasInterfaceNumber)
		}
		if md.BusType != "pcie" {
			t.Fatalf("BusType = %q, want \"pcie\"", md.BusType)
		}
	})
}

//go:build windows && geolocationfixture

package windows

import (
	"github.com/nsgo-dev/nsgo/internal/driver"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"
)

//go:nocheckptr
func testStoreCOM(out uintptr, value *comObject) {
	*(*uintptr)(unsafe.Pointer(out)) = uintptr(unsafe.Pointer(value))
}

//go:nocheckptr
func testInvokeHandler(h *comHandler, status uintptr) {
	syscall.SyscallN(h.vtbl[3], h.ptr(), 0, status)
}

func TestGeolocationLateCompletionAfterCancel(t *testing.T) {
	var puts, cancels, closes, opReleases, locatorReleases atomic.Int32
	var completion *comHandler
	var infoTable, opTable, locatorTable [128]uintptr
	infoTable[2] = syscall.NewCallback(func(uintptr) uintptr { return 0 })
	infoTable[1] = syscall.NewCallback(func(uintptr) uintptr { return 1 })
	infoTable[9] = syscall.NewCallback(func(uintptr) uintptr { cancels.Add(1); return 0 })
	infoTable[10] = syscall.NewCallback(func(uintptr) uintptr { closes.Add(1); return 0 })
	info := &comObject{vtbl: &infoTable}
	opTable[0] = syscall.NewCallback(func(_ uintptr, _ uintptr, out uintptr) uintptr {
		testStoreCOM(out, info)
		info.addRef()
		return 0
	})
	opTable[2] = syscall.NewCallback(func(uintptr) uintptr { opReleases.Add(1); return 0 })
	opTable[1] = syscall.NewCallback(func(uintptr) uintptr { return 1 })
	opTable[6] = syscall.NewCallback(func(_ uintptr, handler uintptr) uintptr {
		completion = (*comHandler)(unsafe.Pointer(handler))
		handlerAddRef(completion)
		puts.Add(1)
		return 0
	})
	op := &comObject{vtbl: &opTable}
	locatorTable[2] = syscall.NewCallback(func(uintptr) uintptr { locatorReleases.Add(1); return 0 })
	locatorTable[13] = syscall.NewCallback(func(_ uintptr, out uintptr) uintptr {
		testStoreCOM(out, op)
		op.call(comAddRef)
		return 0
	})
	locator := &comObject{vtbl: &locatorTable}
	old := activateGeolocator
	activateGeolocator = func() *comObject { return locator }
	defer func() { activateGeolocator = old }()
	w := &window{}
	w.sensorLocation(21, driver.SensorRequest{Kind: driver.SensorLocation, Command: driver.SensorLocate, Generation: 8})
	cancelWindowsLocation(w, 21, 8)
	if completion == nil || puts.Load() != 1 || cancels.Load() != 1 || closes.Load() != 1 || opReleases.Load() != 1 || locatorReleases.Load() != 1 {
		t.Fatalf("setup/cancel counts puts=%d cancels=%d closes=%d opReleases=%d locatorReleases=%d", puts.Load(), cancels.Load(), closes.Load(), opReleases.Load(), locatorReleases.Load())
	}
	// Invoke the real production completion delegate after cancellation. It
	// must observe the removed run and release nothing a second time.
	testInvokeHandler(completion, 2)
	handlerRelease(completion)
	if opReleases.Load() != 1 || locatorReleases.Load() != 1 {
		t.Fatalf("late completion double-released operation=%d locator=%d", opReleases.Load(), locatorReleases.Load())
	}
}

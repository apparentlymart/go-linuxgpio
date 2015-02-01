// Package linuxgpio provides access to the Linux userspace GPIO interface
// (via sysfs).
//
// This implementation should be portable across many Linux-based systems, but
// is unlikely to be as efficient as a native driver for a specific chipset,
// such as go-bcm2835io for the chip on the Raspberry Pi. It also cannot
// configure pull-up and pull-down resistors, as this functionality is not
// exposed via the sysfs interface.
package linuxgpio

import (
	"fmt"
	"github.com/apparentlymart/go-gpio/gpio"
	"os"
	"strconv"
	"syscall"
)

// GpioPin is an extension of gpio.Pin that allows a pin to be closed,
// unexported, etc.
type GpioPin interface {
	gpio.Pin
	gpio.EdgeWaiter

	// Number returns the GPIO number that this instance controls.
	Number() int

	// Close will close the file descriptors that have been opened for this
	// GPIO in sysfs. After this method is called, further use of this instance
	// will fail.
	Close() (err error)

	// Node returns the GpioNode object from which this pin was opened.
	Node() (node GpioNode)
}

var (
	lowData  []byte
	highData []byte
)

func init() {
	lowData = []byte{'0', '\n'}
	highData = []byte{'1', '\n'}
}

// GpioNode represents a Linux GPIO number that may or may not have been
// "exported" into sysfs, and provides an API to export or unexport the
// corresponding GPIO number.
type GpioNode interface {

	// Number returns the GPIO number to which this node belongs.
	Number() int

	// Exported returns true if the GPIO has already been exported and
	// is thus ready to be opened.
	Exported() bool

	// Export asks the kernel to expose the corresponding GPIO into sysfs.
	// A GPIO must be exported before it can be opened, but trying to export
	// a GPIO that has already been exported is an error. Use ExportIfNecessary
	// to export the node only if it is not already exported.
	Export() (err error)

	// ExportIfNecessary is a helper around Exported/Export that attempts to
	// export the selected GPIO only if it's not already exported. However,
	// there is a race condition inherent in this test that may cause it
	// to still fail if another application interacts with the GPIO
	// concurrently.
	//
	// In order to be robust it is not recommended to use this method, and
	// rather to fail if the GPIO is already exported. This is the only way
	// to prevent conflicts with other applications that may be using GPIOs.
	//
	// Returns exported as true if the GPIO wasn't already exported.
	// Applications may wish to unexport only GPIOs that they actually exported
	// when exiting.
	ExportIfNecessary() (exported bool, err error)

	// Unexport asks the kernel to remove the corresponding GPIO from sysfs.
	// Once it has been unexported, any GpioPins created from this node are
	// invalidated and operations on them will fail.
	// It is an error to unexport a GPIO that is not already exported.
	Unexport() (err error)

	// Open the corresponding GPIO so that it can be controlled by the
	// caller.
	Open() (pin GpioPin, err error)
}

// GpioChip represents an instance of a Linux GPIO driver that implements
// zero or more GPIOs on the current system.
//
// Most applications will just hard-code specific GPIO numbers based on
// hardware documentation for the host system, but this interface provides
// a way to implement generic linux GPIO control utilities.
//
// The API to obtain instances of this interface are not yet implemented.
type GpioChip interface {
	FirstGpioNumber() (int, err error)
	GpioCount() (int, err error)
	LastGpioNumber() (int, err error)
	Label() (string, err error)
}

type gpioNode struct {
	number int
	path   string
}

type gpioPin struct {
	node *gpioNode
	dir  *os.File

	// we pre-allocate some storage to avoid creating garbage each time we
	// read a value (which will happen often in many programs) we pre-allocate
	// an array and always read into it. Note however that this means that
	// reading a value is not thread-safe. Worth fixing that?
	readBuf     []byte
	valueFile   *os.File
	epollFd     int
	epollEvents [1]syscall.EpollEvent
}

// MakeGpioNode is the primary way to get hold of a GpioNode object
// representing a particular GPIO. The meaning of the GPIO number varies
// on different host systems; consult the documentation for the host hardware
// to determine appropriate values of "number", or use GpioChips to discover
// which GPIOs are available.
func MakeGpioNode(number int) (node GpioNode) {
	path := fmt.Sprintf("/sys/class/gpio/gpio%d", number)
	return &gpioNode{number: number, path: path}
}

func (node *gpioNode) Exported() (result bool) {
	_, err := os.Stat(node.path)
	return err == nil
}

func (node *gpioNode) Export() (err error) {
	file, err := os.OpenFile("/sys/class/gpio/export", os.O_WRONLY, 0)
	if err != nil {
		return
	}

	_, err = file.WriteString(strconv.Itoa(node.number))
	if err != nil {
		return
	}

	return nil
}

func (node *gpioNode) ExportIfNecessary() (exported bool, err error) {
	if node.Exported() {
		return false, nil
	} else {
		err := node.Export()
		if err == nil {
			return true, nil
		} else {
			return false, err
		}
	}
}

func (node *gpioNode) Unexport() (err error) {
	file, err := os.OpenFile("/sys/class/gpio/unexport", os.O_WRONLY, 0)
	if err != nil {
		return
	}

	_, err = file.WriteString(strconv.Itoa(node.number))
	if err != nil {
		return
	}

	return nil
}

func (node *gpioNode) Number() int {
	return node.number
}

func (node *gpioNode) Open() (GpioPin, error) {
	dir, err := os.Open(node.path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			dir.Close()
		}
	}()

	readBuf := make([]byte, 1, 1)
	pin := &gpioPin{node: node, dir: dir, readBuf: readBuf}

	pin.valueFile, err = pin.openFile("value")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			pin.valueFile.Close()
		}
	}()

	pin.epollFd, err = syscall.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			syscall.Close(pin.epollFd)
		}
	}()

	valueFd := int(pin.valueFile.Fd())

	var event syscall.EpollEvent
	event.Fd = int32(valueFd) // FIXME: will fail on 64-bit systems?
	event.Events = syscall.EPOLLIN | (syscall.EPOLLET & 0xffffffff) | syscall.EPOLLPRI

	err = syscall.EpollCtl(pin.epollFd, syscall.EPOLL_CTL_ADD, valueFd, &event)
	if err != nil {
		return nil, err
	}

	return pin, nil
}

func (pin *gpioPin) Close() (err error) {
	// Try to close whatever we can before checking for errors
	// so that we'll have closed as much as possible before we
	// return. Unfortunately this means the caller won't get the
	// whole picture if multiple things fail, but this is considered
	// and edge case and not worth worrying too much about.
	dirCloseErr := pin.dir.Close()
	fileCloseErr := pin.valueFile.Close()

	switch {
	case dirCloseErr != nil:
		return dirCloseErr
	case fileCloseErr != nil:
		return fileCloseErr
	default:
		return nil
	}
}

func (pin *gpioPin) Node() GpioNode {
	return pin.node
}

func (pin *gpioPin) Number() int {
	return pin.node.number
}

func (pin *gpioPin) openFile(name string) (*os.File, error) {
	fd, err := syscall.Openat(int(pin.dir.Fd()), name, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}

	// It's a little dishonest to pass "name" in here since it's not
	// what we usually expect to find there, but we know we never actually
	// read back name so it doesn't matter.
	return os.NewFile(uintptr(fd), name), nil
}

func (pin *gpioPin) writeFile(name string, value string) error {
	file, err := pin.openFile(name)
	if err != nil {
		return err
	}

	_, err = file.WriteString(value)
	return err
}

func (pin *gpioPin) SetDirection(dir gpio.Direction) error {
	switch dir {
	case gpio.In:
		return pin.writeFile("direction", "in\n")
	case gpio.Out:
		return pin.writeFile("direction", "out\n")
	default:
		// should never happen in a valid program
		panic("Invalid gpio.Direction value")
	}
}

func (pin *gpioPin) SetSensitivity(dir gpio.EdgeSensitivity) error {
	switch dir {
	case gpio.NoEdges:
		return pin.writeFile("edge", "none\n")
	case gpio.RisingEdge:
		return pin.writeFile("edge", "rising\n")
	case gpio.FallingEdge:
		return pin.writeFile("edge", "falling\n")
	case gpio.BothEdges:
		return pin.writeFile("edge", "both\n")
	default:
		// should never happen in a valid program
		panic("Invalid gpio.EdgeSensitivity value")
	}
}

func (pin *gpioPin) WaitForEdge() error {
	_, err := syscall.EpollWait(pin.epollFd, pin.epollEvents[:], -1)
	return err
}

func (pin *gpioPin) SetValue(value gpio.Value) error {
	var err error = nil
	switch value {
	case gpio.High:
		_, err = pin.valueFile.WriteAt(highData, 0)
	case gpio.Low:
		_, err = pin.valueFile.WriteAt(lowData, 0)
	default:
		// should never happen in a valid program
		panic("Invalid gpio.Value value")
	}
	return err
}

func (pin *gpioPin) Value() (gpio.Value, error) {
	bytes, err := pin.valueFile.ReadAt(pin.readBuf, 0)
	if err != nil {
		return 0, err
	}
	if bytes < 1 {
		// should never happen
		panic("Kernel returned nothing from 'value'")
	}

	switch pin.readBuf[0] {
	case '0':
		return gpio.High, nil
	case '1':
		return gpio.Low, nil
	default:
		// should never happen
		panic("Kernel returned invalid data from 'value'")
	}
}

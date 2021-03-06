// Copyright 2018 Klaus Birkelund Abildgaard Jensen
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tape

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"tapr.space/bitmask"
)

// A Serial is the volume serial number (VOLSER) of a tape.
type Serial string

// VolumeCategory represents the volume category.
type VolumeCategory int

// Known volume categories.
const (
	UnknownVolume VolumeCategory = iota
	Allocating
	Allocated
	Scratch
	Filling
	Full
	Missing
	Damaged
	Cleaning
)

const (
	// StatusTransfering denotes that the volume is currently being transfered by the
	// media changer.
	StatusTransfering uint32 = 1 << iota

	// StatusMounted is when the volume is mounted.
	StatusMounted

	// StatusNeedsCleaning tells us that the volume needs cleaning.
	StatusNeedsCleaning

	// StatusFormatted is set when the volume has already been formatted
	StatusFormatted
)

// FormatVolumeFlags formats the flags for human consumption.
func FormatVolumeFlags(f uint32) string {
	var out []string
	if bitmask.IsSet(f, StatusTransfering) {
		out = append(out, "transfering")
	}

	if bitmask.IsSet(f, StatusMounted) {
		out = append(out, "mounted")
	}

	if bitmask.IsSet(f, StatusNeedsCleaning) {
		out = append(out, "needs-cleaning")
	}

	if bitmask.IsSet(f, StatusFormatted) {
		out = append(out, "formatted")
	}

	str := strings.Join(out, ",")
	if str == "" {
		str = "none"
	}

	return str
}

// A Volume is an usable volume.
type Volume struct {
	// Serial is the Volume Serial (VOLSER).
	Serial Serial

	// Location is the current location in the store.
	Location Location

	// Home is the home location in the store.
	Home Location

	// Category tracks the volume state.
	Category VolumeCategory

	// Flags are contains temporary info on the volume.
	Flags uint32
}

func (v *Volume) String() string {
	return fmt.Sprintf("[%v %v (loc: %v) (home: %v) (flags: %s)]",
		v.Serial, v.Category, v.Location, v.Home, FormatVolumeFlags(v.Flags),
	)
}

// String implements fmt.Stringer.
func (cat VolumeCategory) String() string {
	switch cat {
	case UnknownVolume:
		return "unknown"
	case Allocating:
		return "allocating"
	case Allocated:
		return "allocated"
	case Scratch:
		return "scratch"
	case Filling:
		return "filling"
	case Full:
		return "full"
	case Missing:
		return "missing"
	case Damaged:
		return "damaged"
	case Cleaning:
		return "cleaning"
	}

	panic("unknown volume category")
}

// ToVolumeCategory returns the VolumeStatus corresponding to the given string.
func ToVolumeCategory(str string) VolumeCategory {
	switch str {
	case "unknown":
		return UnknownVolume
	case "allocating":
		return Allocating
	case "allocated":
		return Allocated
	case "scratch":
		return Scratch
	case "filling":
		return Filling
	case "full":
		return Full
	case "missing":
		return Missing
	case "damaged":
		return Damaged
	case "cleaning":
		return Cleaning
	default:
		panic("unknown volume category")
	}
}

// Value implements driver.Valuer.
func (cat VolumeCategory) Value() (driver.Value, error) {
	return cat.String(), nil
}

// Scan implements sql.Scanner.
func (cat *VolumeCategory) Scan(src interface{}) error {
	*cat = ToVolumeCategory(string(src.([]byte)))

	return nil
}

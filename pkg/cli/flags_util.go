// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cli

import (
	gohex "encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/status"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/keysutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/redact"
	humanize "github.com/dustin/go-humanize"
	"github.com/spf13/pflag"
)

type localityList []roachpb.LocalityAddress

var _ pflag.Value = &localityList{}

// Type implements the pflag.Value interface.
func (l *localityList) Type() string { return "localityList" }

// String implements the pflag.Value interface.
func (l *localityList) String() string {
	string := ""
	for _, loc := range []roachpb.LocalityAddress(*l) {
		string += loc.LocalityTier.Key + "=" + loc.LocalityTier.Value + "@" + loc.Address.String() + ","
	}

	return string
}

// Set implements the pflag.Value interface.
func (l *localityList) Set(value string) error {
	*l = []roachpb.LocalityAddress{}

	values := strings.Split(value, ",")

	for _, value := range values {
		split := strings.Split(value, "@")
		if len(split) != 2 {
			return fmt.Errorf("invalid value for --locality-advertise-address: %s", l)
		}

		tierSplit := strings.Split(split[0], "=")
		if len(tierSplit) != 2 {
			return fmt.Errorf("invalid value for --locality-advertise-address: %s", l)
		}

		tier := roachpb.Tier{}
		tier.Key = tierSplit[0]
		tier.Value = tierSplit[1]

		locAddress := roachpb.LocalityAddress{}
		locAddress.LocalityTier = tier
		locAddress.Address = util.MakeUnresolvedAddr("tcp", split[1])

		*l = append(*l, locAddress)
	}

	return nil
}

// This file contains definitions for data types suitable for use by
// the flag+pflag packages.

type dumpMode int

const (
	dumpBoth dumpMode = iota
	dumpSchemaOnly
	dumpDataOnly
)

// Type implements the pflag.Value interface.
func (m *dumpMode) Type() string { return "string" }

// String implements the pflag.Value interface.
func (m *dumpMode) String() string {
	switch *m {
	case dumpBoth:
		return "both"
	case dumpSchemaOnly:
		return "schema"
	case dumpDataOnly:
		return "data"
	}
	return ""
}

// Set implements the pflag.Value interface.
func (m *dumpMode) Set(s string) error {
	switch s {
	case "both":
		*m = dumpBoth
	case "schema":
		*m = dumpSchemaOnly
	case "data":
		*m = dumpDataOnly
	default:
		return fmt.Errorf("invalid value for --dump-mode: %s", s)
	}
	return nil
}

type mvccKey storage.MVCCKey

// Type implements the pflag.Value interface.
func (k *mvccKey) Type() string { return "engine.MVCCKey" }

// String implements the pflag.Value interface.
func (k *mvccKey) String() string {
	return storage.MVCCKey(*k).String()
}

// Set implements the pflag.Value interface.
func (k *mvccKey) Set(value string) error {
	var typ keyType
	var keyStr string
	i := strings.IndexByte(value, ':')
	if i == -1 {
		keyStr = value
	} else {
		var err error
		typ, err = parseKeyType(value[:i])
		if err != nil {
			return err
		}
		keyStr = value[i+1:]
	}

	switch typ {
	case hex:
		b, err := gohex.DecodeString(keyStr)
		if err != nil {
			return err
		}
		newK, err := storage.DecodeMVCCKey(b)
		if err != nil {
			encoded := gohex.EncodeToString(storage.EncodeMVCCKey(storage.MakeMVCCMetadataKey(roachpb.Key(b))))
			return errors.Wrapf(err, "perhaps this is just a hex-encoded key; you need an "+
				"encoded MVCCKey (i.e. with a timestamp component); here's one with a zero timestamp: %s",
				encoded)
		}
		*k = mvccKey(newK)
	case raw:
		unquoted, err := unquoteArg(keyStr)
		if err != nil {
			return err
		}
		*k = mvccKey(storage.MakeMVCCMetadataKey(roachpb.Key(unquoted)))
	case human:
		scanner := keysutil.MakePrettyScanner(nil /* tableParser */, nil /* tenantParser */)
		key, err := scanner.Scan(keyStr)
		if err != nil {
			return err
		}
		*k = mvccKey(storage.MakeMVCCMetadataKey(key))
	case rangeID:
		fromID, err := parseRangeID(keyStr)
		if err != nil {
			return err
		}
		*k = mvccKey(storage.MakeMVCCMetadataKey(keys.MakeRangeIDPrefix(fromID)))
	default:
		return fmt.Errorf("unknown key type %s", typ)
	}

	return nil
}

// unquoteArg unquotes the provided argument using Go double-quoted
// string literal rules.
func unquoteArg(arg string) (string, error) {
	s, err := strconv.Unquote(`"` + arg + `"`)
	if err != nil {
		return "", errors.Wrapf(err, "invalid argument %q", arg)
	}
	return s, nil
}

type keyType int

//go:generate stringer -type=keyType
const (
	raw keyType = iota
	human
	rangeID
	hex
)

func parseKeyType(value string) (keyType, error) {
	for typ, i := range _keyTypes {
		if strings.EqualFold(value, typ) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown key type '%s'", value)
}

type nodeDecommissionWaitType int

const (
	nodeDecommissionWaitAll nodeDecommissionWaitType = iota
	nodeDecommissionWaitNone
)

// Type implements the pflag.Value interface.
func (s *nodeDecommissionWaitType) Type() string { return "string" }

// String implements the pflag.Value interface.
func (s *nodeDecommissionWaitType) String() string {
	switch *s {
	case nodeDecommissionWaitAll:
		return "all"
	case nodeDecommissionWaitNone:
		return "none"
	default:
		panic("unexpected node decommission wait type (possible values: all, none)")
	}
}

// Set implements the pflag.Value interface.
func (s *nodeDecommissionWaitType) Set(value string) error {
	switch value {
	case "all":
		*s = nodeDecommissionWaitAll
	case "none":
		*s = nodeDecommissionWaitNone
	default:
		return fmt.Errorf("invalid node decommission parameter: %s "+
			"(possible values: all, none)", value)
	}
	return nil
}

type nodeDecommissionCheckMode int

const (
	nodeDecommissionChecksSkip nodeDecommissionCheckMode = iota
	nodeDecommissionChecksEnabled
	nodeDecommissionChecksStrict
)

// Type implements the pflag.Value interface.
func (s *nodeDecommissionCheckMode) Type() string { return "string" }

// String implements the pflag.Value interface.
func (s *nodeDecommissionCheckMode) String() string {
	switch *s {
	case nodeDecommissionChecksSkip:
		return "skip"
	case nodeDecommissionChecksEnabled:
		return "enabled"
	case nodeDecommissionChecksStrict:
		return "strict"
	default:
		panic("unexpected node decommission check mode (possible values: enabled, strict, skip)")
	}
}

// Set implements the pflag.Value interface.
func (s *nodeDecommissionCheckMode) Set(value string) error {
	switch value {
	case "skip":
		*s = nodeDecommissionChecksSkip
	case "enabled":
		*s = nodeDecommissionChecksEnabled
	case "strict":
		*s = nodeDecommissionChecksStrict
	default:
		return fmt.Errorf("invalid node decommission parameter: %s "+
			"(possible values: enabled, strict, skip)", value)
	}
	return nil
}

// bytesOrPercentageValue is a flag that accepts an integer value, an integer
// plus a unit (e.g. 32GB or 32GiB) or a percentage (e.g. 32%). In all these
// cases, it transforms the string flag input into an int64 value.
//
// Since it accepts a percentage, instances need to be configured with
// instructions on how to resolve a percentage to a number (i.e. the answer to
// the question "a percentage of what?"). This is done by taking in a
// percentResolverFunc. There are predefined ones: memoryPercentResolver and
// diskPercentResolverFactory.
//
// bytesOrPercentageValue can be used in two ways:
// 1. Upon flag parsing, it can write an int64 value through a pointer specified
// by the caller.
// 2. It can store the flag value as a string and only convert it to an int64 on
// a subsequent Resolve() call. Input validation still happens at flag parsing
// time.
//
// Option 2 is useful when percentages cannot be resolved at flag parsing time.
// For example, we have flags that can be expressed as percentages of the
// capacity of storage device. Which storage device is in question might only be
// known once other flags are parsed (e.g. --max-disk-temp-storage=10% depends
// on --store).
type bytesOrPercentageValue struct {
	bval *humanizeutil.BytesValue

	origVal string

	// percentResolver is used to turn a percent string into a value. See
	// memoryPercentResolver() and diskPercentResolverFactory().
	percentResolver percentResolverFunc
}

var _ redact.SafeFormatter = (*bytesOrPercentageValue)(nil)

type percentResolverFunc func(percent int) (int64, error)

// memoryPercentResolver turns a percent into the respective fraction of the
// system's internal memory.
func memoryPercentResolver(percent int) (int64, error) {
	sizeBytes, _, err := status.GetTotalMemoryWithoutLogging()
	if err != nil {
		return 0, err
	}
	return (sizeBytes * int64(percent)) / 100, nil
}

// diskPercentResolverFactory takes in a path and produces a percentResolverFunc
// bound to the respective storage device.
//
// An error is returned if dir does not exist.
func diskPercentResolverFactory(dir string) (percentResolverFunc, error) {
	du, err := vfs.Default.GetDiskUsage(dir)
	if err != nil {
		return nil, err
	}
	if du.TotalBytes > math.MaxInt64 {
		return nil, fmt.Errorf("unsupported disk size %s, max supported size is %s",
			humanize.IBytes(du.TotalBytes), humanizeutil.IBytes(math.MaxInt64))
	}
	deviceCapacity := int64(du.TotalBytes)

	return func(percent int) (int64, error) {
		return (deviceCapacity * int64(percent)) / 100, nil
	}, nil
}

// makeBytesOrPercentageValue creates a bytesOrPercentageValue.
//
// v and percentResolver can be nil (either they're both specified or they're
// both nil). If they're nil, then Resolve() has to be called later to get the
// passed-in value.
//
// When using this function, be sure to define the flag Value in a
// context struct (in context.go) and place the call to
// makeBytesOrPercentageValue() in one of the context init
// functions. Do not use global-scope variables.
func makeBytesOrPercentageValue(
	v *int64, percentResolver func(percent int) (int64, error),
) bytesOrPercentageValue {
	return bytesOrPercentageValue{
		bval:            humanizeutil.NewBytesValue(v),
		percentResolver: percentResolver,
	}
}

var fractionRE = regexp.MustCompile(`^0?\.[0-9]+$`)

// Set implements the pflags.Flag interface.
func (b *bytesOrPercentageValue) Set(s string) error {
	b.origVal = s
	if strings.HasSuffix(s, "%") || fractionRE.MatchString(s) {
		multiplier := 100.0
		if s[len(s)-1] == '%' {
			// We have a percentage.
			multiplier = 1.0
			s = s[:len(s)-1]
		}
		// The user can express .123 or 0.123. Parse as float.
		frac, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return err
		}
		percent := int(frac * multiplier)
		if percent < 1 || percent > 99 {
			return fmt.Errorf("percentage %d%% out of range 1%% - 99%%", percent)
		}

		if b.percentResolver == nil {
			// percentResolver not set means that this flag is not yet supposed to set
			// any value.
			return nil
		}

		absVal, err := b.percentResolver(percent)
		if err != nil {
			return err
		}
		s = fmt.Sprint(absVal)
	}
	return b.bval.Set(s)
}

// Resolve can be called to get the flag's value (if any). If the flag had been
// previously set, *v will be written.
func (b *bytesOrPercentageValue) Resolve(v *int64, percentResolver percentResolverFunc) error {
	// The flag was not passed on the command line.
	if b.origVal == "" {
		return nil
	}
	b.percentResolver = percentResolver
	b.bval = humanizeutil.NewBytesValue(v)
	return b.Set(b.origVal)
}

// Type implements the pflag.Value interface.
func (b *bytesOrPercentageValue) Type() string {
	return b.bval.Type()
}

// String implements the pflag.Value interface.
func (b *bytesOrPercentageValue) String() string {
	return redact.StringWithoutMarkers(b)
}

// SafeFormat implements the redact.SafeFormatter interface.
func (b *bytesOrPercentageValue) SafeFormat(p redact.SafePrinter, _ rune) {
	p.Print(b.bval)
}

// IsSet returns true iff Set has successfully been called.
func (b *bytesOrPercentageValue) IsSet() bool {
	return b.bval.IsSet()
}

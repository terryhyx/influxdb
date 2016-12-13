package tsi1

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/influxdata/influxdb/pkg/estimator/hll"

	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/bytesutil"
	"github.com/influxdata/influxdb/pkg/estimator"
	"github.com/influxdata/influxdb/tsdb"
)

func init() {
	tsdb.RegisterIndex("tsi1", func(id uint64, path string, opt tsdb.EngineOptions) tsdb.Index {
		return &Index{Path: path}
	})
}

// File extensions.
const (
	LogFileExt   = ".tsi.log"
	IndexFileExt = ".tsi"
)

// Ensure index implements the interface.
var _ tsdb.Index = &Index{}

// Index represents a collection of layered index files and WAL.
type Index struct {
	Path string

	mu         sync.RWMutex
	logFiles   []*LogFile
	indexFiles IndexFiles

	// Fieldset shared with engine.
	// TODO: Move field management into index.
	fieldset *tsdb.MeasurementFieldSet
}

// Open opens the index.
func (i *Index) Open() error {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Open root index directory.
	f, err := os.Open(i.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Open all log & index files.
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		switch filepath.Ext(name) {
		case LogFileExt:
			if err := i.openLogFile(name); err != nil {
				return err
			}
		case IndexFileExt:
			if err := i.openIndexFile(name); err != nil {
				return err
			}
		}
	}

	// Ensure at least one log file exists.
	if len(i.logFiles) == 0 {
		path := filepath.Join(i.Path, fmt.Sprintf("%08x%s", 0, LogFileExt))
		if err := i.openLogFile(path); err != nil {
			return err
		}
	}

	return nil
}

// openLogFile opens a log file and appends it to the index.
func (i *Index) openLogFile(path string) error {
	f := NewLogFile()
	f.Path = path
	if err := f.Open(); err != nil {
		return err
	}

	i.logFiles = append(i.logFiles, f)
	return nil
}

// openIndexFile opens a log file and appends it to the index.
func (i *Index) openIndexFile(path string) error {
	f := NewIndexFile()
	f.Path = path
	if err := f.Open(); err != nil {
		return err
	}

	i.indexFiles = append(i.indexFiles, f)
	return nil
}

// Close closes the index.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Close log files.
	for _, f := range i.logFiles {
		f.Close()
	}
	i.logFiles = nil

	// Close index files.
	for _, f := range i.indexFiles {
		f.Close()
	}
	i.indexFiles = nil

	return nil
}

// SetFieldSet sets a shared field set from the engine.
func (i *Index) SetFieldSet(fs *tsdb.MeasurementFieldSet) {
	i.mu.Lock()
	i.fieldset = fs
	i.mu.Unlock()
}

// SetLogFiles explicitly sets log files.
// TEMPORARY: For testing only.
func (i *Index) SetLogFiles(a ...*LogFile) { i.logFiles = a }

// SetIndexFiles explicitly sets index files
// TEMPORARY: For testing only.
func (i *Index) SetIndexFiles(a ...*IndexFile) { i.indexFiles = IndexFiles(a) }

// FileN returns the number of log and index files within the index.
func (i *Index) FileN() int { return len(i.logFiles) + len(i.indexFiles) }

// files returns a list of all log & index files.
//
// OPTIMIZE(benbjohnson): Convert to an iterator to remove allocation.
func (i *Index) files() []File {
	a := make([]File, 0, len(i.logFiles)+len(i.indexFiles))
	for _, f := range i.logFiles {
		a = append(a, f)
	}
	for _, f := range i.indexFiles {
		a = append(a, f)
	}
	return a
}

// SeriesIterator returns an iterator over all series in the index.
func (i *Index) SeriesIterator() SeriesIterator {
	a := make([]SeriesIterator, 0, i.FileN())
	for _, f := range i.files() {
		itr := f.SeriesIterator()
		if itr == nil {
			continue
		}
		a = append(a, itr)
	}
	return FilterUndeletedSeriesIterator(MergeSeriesIterators(a...))
}

// Measurement retrieves a measurement by name.
func (i *Index) Measurement(name []byte) (*tsdb.Measurement, error) {
	return i.measurement(name), nil
}

func (i *Index) measurement(name []byte) *tsdb.Measurement {
	m := tsdb.NewMeasurement(string(name))

	// Iterate over measurement series.
	itr := i.MeasurementSeriesIterator(name)

	var id uint64 // TEMPORARY
	for e := itr.Next(); e != nil; e = itr.Next() {
		// TODO: Handle deleted series.

		// Append series to to measurement.
		// TODO: Remove concept of series ids.
		m.AddSeries(&tsdb.Series{
			ID:   id,
			Key:  string(e.Name()),
			Tags: models.CopyTags(e.Tags()),
		})

		// TEMPORARY: Increment ID.
		id++
	}

	if !m.HasSeries() {
		return nil
	}
	return m
}

// ForEachMeasurement iterates over all measurements in the index.
func (i *Index) ForEachMeasurement(fn func(name []byte) error) error {
	itr := i.MeasurementIterator()
	if itr == nil {
		return nil
	}

	for e := itr.Next(); e != nil; e = itr.Next() {
		if err := fn(e.Name()); err != nil {
			return err
		}
	}

	return nil
}

// MeasurementSeriesIterator returns an iterator over all series in the index.
func (i *Index) MeasurementSeriesIterator(name []byte) SeriesIterator {
	a := make([]SeriesIterator, 0, i.FileN())
	for _, f := range i.files() {
		itr := f.MeasurementSeriesIterator(name)
		if itr != nil {
			a = append(a, itr)
		}
	}
	return FilterUndeletedSeriesIterator(MergeSeriesIterators(a...))
}

// TagKeyIterator returns an iterator over all tag keys for a measurement.
func (i *Index) TagKeyIterator(name []byte) TagKeyIterator {
	a := make([]TagKeyIterator, 0, i.FileN())
	for _, f := range i.files() {
		itr := f.TagKeyIterator(name)
		if itr != nil {
			a = append(a, itr)
		}
	}
	return MergeTagKeyIterators(a...)
}

// Measurements returns a list of all measurements.
func (i *Index) Measurements() (tsdb.Measurements, error) {
	var mms tsdb.Measurements
	itr := i.MeasurementIterator()
	for e := itr.Next(); e != nil; e = itr.Next() {
		mms = append(mms, i.measurement(e.Name()))
	}
	return mms, nil
}

// MeasurementIterator returns an iterator over all measurements in the index.
func (i *Index) MeasurementIterator() MeasurementIterator {
	a := make([]MeasurementIterator, 0, i.FileN())
	for _, f := range i.logFiles {
		a = append(a, f.MeasurementIterator())
	}
	for _, f := range i.indexFiles {
		a = append(a, f.MeasurementIterator())
	}
	return FilterUndeletedMeasurementIterator(MergeMeasurementIterators(a...))
}

func (i *Index) MeasurementNamesByExpr(expr influxql.Expr) ([][]byte, error) {
	// Return filtered list if expression exists.
	if expr != nil {
		return i.measurementNamesByExpr(expr)
	}

	// Iterate over all measurements if no condition exists.
	var names [][]byte
	itr := i.MeasurementIterator()
	for e := itr.Next(); e != nil; e = itr.Next() {
		names = append(names, e.Name())
	}
	return names, nil
}

func (i *Index) measurementNamesByExpr(expr influxql.Expr) ([][]byte, error) {
	if expr == nil {
		return nil, nil
	}

	switch e := expr.(type) {
	case *influxql.BinaryExpr:
		switch e.Op {
		case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
			tag, ok := e.LHS.(*influxql.VarRef)
			if !ok {
				return nil, fmt.Errorf("left side of '%s' must be a tag key", e.Op.String())
			}

			// Retrieve value or regex expression from RHS.
			var value string
			var regex *regexp.Regexp
			if influxql.IsRegexOp(e.Op) {
				re, ok := e.RHS.(*influxql.RegexLiteral)
				if !ok {
					return nil, fmt.Errorf("right side of '%s' must be a regular expression", e.Op.String())
				}
				regex = re.Val
			} else {
				s, ok := e.RHS.(*influxql.StringLiteral)
				if !ok {
					return nil, fmt.Errorf("right side of '%s' must be a tag value string", e.Op.String())
				}
				value = s.Val
			}

			// Match on name, if specified.
			if tag.Val == "_name" {
				return i.measurementNamesByNameFilter(e.Op, value, regex), nil
			} else if influxql.IsSystemName(tag.Val) {
				return nil, nil
			}
			return i.measurementNamesByTagFilter(e.Op, tag.Val, value, regex), nil

		case influxql.OR, influxql.AND:
			lhs, err := i.measurementNamesByExpr(e.LHS)
			if err != nil {
				return nil, err
			}

			rhs, err := i.measurementNamesByExpr(e.RHS)
			if err != nil {
				return nil, err
			}

			if e.Op == influxql.OR {
				return bytesutil.Union(lhs, rhs), nil
			}
			return bytesutil.Intersect(lhs, rhs), nil

		default:
			return nil, fmt.Errorf("invalid tag comparison operator")
		}

	case *influxql.ParenExpr:
		return i.measurementNamesByExpr(e.Expr)
	default:
		return nil, fmt.Errorf("%#v", expr)
	}
}

// measurementNamesByNameFilter returns matching measurement names in sorted order.
func (i *Index) measurementNamesByNameFilter(op influxql.Token, val string, regex *regexp.Regexp) [][]byte {
	var names [][]byte
	itr := i.MeasurementIterator()
	for e := itr.Next(); e != nil; e = itr.Next() {
		var matched bool
		switch op {
		case influxql.EQ:
			matched = string(e.Name()) == val
		case influxql.NEQ:
			matched = string(e.Name()) != val
		case influxql.EQREGEX:
			matched = regex.Match(e.Name())
		case influxql.NEQREGEX:
			matched = !regex.Match(e.Name())
		}

		if matched {
			names = append(names, e.Name())
		}
	}
	bytesutil.Sort(names)
	return names
}

func (i *Index) measurementNamesByTagFilter(op influxql.Token, key, val string, regex *regexp.Regexp) [][]byte {
	var names [][]byte

	mitr := i.MeasurementIterator()
	for me := mitr.Next(); me != nil; me = mitr.Next() {
		// If the operator is non-regex, only check the specified value.
		var tagMatch bool
		if op == influxql.EQ || op == influxql.NEQ {
			if i.HasTagValue(me.Name(), []byte(key), []byte(val)) {
				tagMatch = true
			}
		} else {
			// Else, the operator is a regex and we have to check all tag
			// values against the regular expression.
			vitr := i.TagValueIterator(me.Name(), []byte(key))
			for ve := vitr.Next(); ve != nil; ve = vitr.Next() {
				if regex.Match(ve.Value()) {
					tagMatch = true
					break
				}
			}
		}

		//
		// XNOR gate
		//
		// tags match | operation is EQ | measurement matches
		// --------------------------------------------------
		//     True   |       True      |      True
		//     True   |       False     |      False
		//     False  |       True      |      False
		//     False  |       False     |      True
		if tagMatch == (op == influxql.EQ || op == influxql.EQREGEX) {
			names = append(names, me.Name())
			continue
		}
	}

	bytesutil.Sort(names)
	return names
}

func (i *Index) MeasurementNamesByRegex(re *regexp.Regexp) ([][]byte, error) {
	itr := i.MeasurementIterator()
	var a [][]byte
	for e := itr.Next(); e != nil; e = itr.Next() {
		if re.Match(e.Name()) {
			a = append(a, e.Name())
		}
	}
	return a, nil
}

// DropMeasurement deletes a measurement from the index.
func (i *Index) DropMeasurement(name []byte) error {
	// Delete all keys and values.
	if kitr := i.TagKeyIterator(name); kitr != nil {
		for k := kitr.Next(); k != nil; k = kitr.Next() {
			// Delete key if not already deleted.
			if !k.Deleted() {
				if err := i.logFiles[0].DeleteTagKey(name, k.Key()); err != nil {
					return err
				}
			}

			// Delete each value in key.
			if vitr := k.TagValueIterator(); vitr != nil {
				for v := vitr.Next(); v != nil; v = vitr.Next() {
					if !v.Deleted() {
						if err := i.logFiles[0].DeleteTagValue(name, k.Key(), v.Value()); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	// Delete all series in measurement.
	if sitr := i.MeasurementSeriesIterator(name); sitr != nil {
		for s := sitr.Next(); s != nil; s = sitr.Next() {
			if !s.Deleted() {
				if err := i.logFiles[0].DeleteSeries(s.Name(), s.Tags()); err != nil {
					return err
				}
			}
		}
	}

	// Mark measurement as deleted.
	return i.logFiles[0].DeleteMeasurement(name)
}

// CreateSeriesIfNotExists creates a series if it doesn't exist or is deleted.
func (i *Index) CreateSeriesIfNotExists(key, name []byte, tags models.Tags) error {
	if e := i.Series(name, tags); e != nil {
		return nil
	}
	return i.logFiles[0].AddSeries(name, tags)
}

// Series returns a series by name/tags.
func (i *Index) Series(name []byte, tags models.Tags) SeriesElem {
	for _, f := range i.files() {
		if e := f.Series(name, tags); e != nil && !e.Deleted() {
			return e
		}
	}
	return nil
}

func (i *Index) DropSeries(keys [][]byte) error {
	for _, key := range keys {
		name, tags, err := models.ParseKey(key)
		if err != nil {
			return err
		}

		if err := i.logFiles[0].DeleteSeries([]byte(name), tags); err != nil {
			return err
		}
	}
	return nil
}

func (i *Index) SeriesN() (n uint64, err error) {
	// FIXME(edd): Use sketches.

	// HACK(benbjohnson): Use first log file until edd adds sketches.
	return i.logFiles[0].SeriesN(), nil
}

func (i *Index) sketches(nextSketches func(*IndexFile) (estimator.Sketch, estimator.Sketch)) (estimator.Sketch, estimator.Sketch, error) {
	sketch, tsketch := hll.NewDefaultPlus(), hll.NewDefaultPlus()

	// Iterate over all the index files and merge all the sketches.
	for _, f := range i.indexFiles {
		s, t := nextSketches(f)
		if err := sketch.Merge(s); err != nil {
			return nil, nil, err
		}

		if err := tsketch.Merge(t); err != nil {
			return nil, nil, err
		}
	}
	return sketch, tsketch, nil
}

// SeriesSketches returns the two sketches for the index by merging all
// instances of the type sketch types in all the indexes files.
func (i *Index) SeriesSketches() (estimator.Sketch, estimator.Sketch, error) {
	sketch, tsketch, err := i.sketches(func(i *IndexFile) (estimator.Sketch, estimator.Sketch) {
		return i.sblk.sketch, i.sblk.tsketch
	})

	if err != nil {
		return nil, nil, err
	}

	// Merge in any current log file sketches.
	for _, f := range i.logFiles {
		if err := sketch.Merge(f.sSketch); err != nil {
			return nil, nil, err
		}
		if err := tsketch.Merge(f.sTSketch); err != nil {
			return nil, nil, err
		}
	}
	return sketch, tsketch, err
}

// MeasurementsSketches returns the two sketches for the index by merging all
// instances of the type sketch types in all the indexes files.
func (i *Index) MeasurementsSketches() (estimator.Sketch, estimator.Sketch, error) {
	sketch, tsketch, err := i.sketches(func(i *IndexFile) (estimator.Sketch, estimator.Sketch) {
		return i.mblk.sketch, i.mblk.tsketch
	})

	if err != nil {
		return nil, nil, err
	}

	// Merge in any current log file sketches.
	for _, f := range i.logFiles {
		if err := sketch.Merge(f.mSketch); err != nil {
			return nil, nil, err
		}
		if err := tsketch.Merge(f.mTSketch); err != nil {
			return nil, nil, err
		}
	}
	return sketch, tsketch, err
}

// Dereference is a nop.
func (i *Index) Dereference([]byte) {}

// MeasurementTagKeysByExpr extracts the tag keys wanted by the expression.
func (i *Index) MeasurementTagKeysByExpr(name []byte, expr influxql.Expr) (map[string]struct{}, error) {
	switch e := expr.(type) {
	case *influxql.BinaryExpr:
		switch e.Op {
		case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
			tag, ok := e.LHS.(*influxql.VarRef)
			if !ok {
				return nil, fmt.Errorf("left side of '%s' must be a tag key", e.Op.String())
			} else if tag.Val != "_tagKey" {
				return nil, nil
			}

			if influxql.IsRegexOp(e.Op) {
				re, ok := e.RHS.(*influxql.RegexLiteral)
				if !ok {
					return nil, fmt.Errorf("right side of '%s' must be a regular expression", e.Op.String())
				}
				return i.tagKeysByFilter(name, e.Op, nil, re.Val), nil
			}

			s, ok := e.RHS.(*influxql.StringLiteral)
			if !ok {
				return nil, fmt.Errorf("right side of '%s' must be a tag value string", e.Op.String())
			}
			return i.tagKeysByFilter(name, e.Op, []byte(s.Val), nil), nil

		case influxql.AND, influxql.OR:
			lhs, err := i.MeasurementTagKeysByExpr(name, e.LHS)
			if err != nil {
				return nil, err
			}

			rhs, err := i.MeasurementTagKeysByExpr(name, e.RHS)
			if err != nil {
				return nil, err
			}

			if lhs != nil && rhs != nil {
				if e.Op == influxql.OR {
					return unionStringSets(lhs, rhs), nil
				}
				return intersectStringSets(lhs, rhs), nil
			} else if lhs != nil {
				return lhs, nil
			} else if rhs != nil {
				return rhs, nil
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("invalid operator")
		}

	case *influxql.ParenExpr:
		return i.MeasurementTagKeysByExpr(name, e.Expr)
	}

	return nil, fmt.Errorf("%#v", expr)
}

// tagKeysByFilter will filter the tag keys for the measurement.
func (i *Index) tagKeysByFilter(name []byte, op influxql.Token, val []byte, regex *regexp.Regexp) map[string]struct{} {
	ss := make(map[string]struct{})
	itr := i.TagKeyIterator(name)
	for e := itr.Next(); e != nil; e = itr.Next() {
		var matched bool
		switch op {
		case influxql.EQ:
			matched = bytes.Equal(e.Key(), val)
		case influxql.NEQ:
			matched = !bytes.Equal(e.Key(), val)
		case influxql.EQREGEX:
			matched = regex.Match(e.Key())
		case influxql.NEQREGEX:
			matched = !regex.Match(e.Key())
		}

		if !matched {
			continue
		}
		ss[string(e.Key())] = struct{}{}
	}
	return ss
}

// TagKeySeriesIterator returns a series iterator for all values across a single key.
func (i *Index) TagKeySeriesIterator(name, key []byte) SeriesIterator {
	a := make([]SeriesIterator, 0, i.FileN())
	for _, f := range i.files() {
		itr := f.TagKeySeriesIterator(name, key)
		if itr != nil {
			a = append(a, itr)
		}
	}
	return FilterUndeletedSeriesIterator(MergeSeriesIterators(a...))
}

// HasTagValue returns true if the tag value exists.
func (i *Index) HasTagValue(name, key, value []byte) bool {
	for _, f := range i.files() {
		if e := f.TagValue(name, key, value); e != nil {
			return !e.Deleted()
		}
	}
	return false
}

// TagValueIterator returns a value iterator for a tag key.
func (i *Index) TagValueIterator(name, key []byte) TagValueIterator {
	a := make([]TagValueIterator, 0, i.FileN())
	for _, f := range i.files() {
		itr := f.TagValueIterator(name, key)
		if itr != nil {
			a = append(a, itr)
		}
	}
	return MergeTagValueIterators(a...)
}

// TagValueSeriesIterator returns a series iterator for a single tag value.
func (i *Index) TagValueSeriesIterator(name, key, value []byte) SeriesIterator {
	a := make([]SeriesIterator, 0, i.FileN())
	for _, f := range i.files() {
		itr := f.TagValueSeriesIterator(name, key, value)
		if itr != nil {
			a = append(a, itr)
		}
	}
	return FilterUndeletedSeriesIterator(MergeSeriesIterators(a...))
}

// MatchTagValueSeriesIterator returns a series iterator for tags which match value.
// If matches is false, returns iterators which do not match value.
func (i *Index) MatchTagValueSeriesIterator(name, key []byte, value *regexp.Regexp, matches bool) SeriesIterator {
	matchEmpty := value.MatchString("")

	if matches {
		if matchEmpty {
			return FilterUndeletedSeriesIterator(i.matchTagValueEqualEmptySeriesIterator(name, key, value))
		}
		return FilterUndeletedSeriesIterator(i.matchTagValueEqualNotEmptySeriesIterator(name, key, value))
	}

	if matchEmpty {
		return FilterUndeletedSeriesIterator(i.matchTagValueNotEqualEmptySeriesIterator(name, key, value))
	}
	return FilterUndeletedSeriesIterator(i.matchTagValueNotEqualNotEmptySeriesIterator(name, key, value))
}

func (i *Index) matchTagValueEqualEmptySeriesIterator(name, key []byte, value *regexp.Regexp) SeriesIterator {
	vitr := i.TagValueIterator(name, key)
	if vitr == nil {
		return i.MeasurementSeriesIterator(name)
	}

	var itrs []SeriesIterator
	for e := vitr.Next(); e != nil; e = vitr.Next() {
		if !value.Match(e.Value()) {
			itrs = append(itrs, i.TagValueSeriesIterator(name, key, e.Value()))
		}
	}

	return DifferenceSeriesIterators(
		i.MeasurementSeriesIterator(name),
		MergeSeriesIterators(itrs...),
	)
}

func (i *Index) matchTagValueEqualNotEmptySeriesIterator(name, key []byte, value *regexp.Regexp) SeriesIterator {
	vitr := i.TagValueIterator(name, key)
	if vitr == nil {
		return nil
	}

	var itrs []SeriesIterator
	for e := vitr.Next(); e != nil; e = vitr.Next() {
		if value.Match(e.Value()) {
			itrs = append(itrs, i.TagValueSeriesIterator(name, key, e.Value()))
		}
	}
	return MergeSeriesIterators(itrs...)
}

func (i *Index) matchTagValueNotEqualEmptySeriesIterator(name, key []byte, value *regexp.Regexp) SeriesIterator {
	vitr := i.TagValueIterator(name, key)
	if vitr == nil {
		return nil
	}

	var itrs []SeriesIterator
	for e := vitr.Next(); e != nil; e = vitr.Next() {
		if !value.Match(e.Value()) {
			itrs = append(itrs, i.TagValueSeriesIterator(name, key, e.Value()))
		}
	}
	return MergeSeriesIterators(itrs...)
}

func (i *Index) matchTagValueNotEqualNotEmptySeriesIterator(name, key []byte, value *regexp.Regexp) SeriesIterator {
	vitr := i.TagValueIterator(name, key)
	if vitr == nil {
		return i.MeasurementSeriesIterator(name)
	}

	var itrs []SeriesIterator
	for e := vitr.Next(); e != nil; e = vitr.Next() {
		if value.Match(e.Value()) {
			itrs = append(itrs, i.TagValueSeriesIterator(name, key, e.Value()))
		}
	}

	return DifferenceSeriesIterators(
		i.MeasurementSeriesIterator(name),
		MergeSeriesIterators(itrs...),
	)
}

// ForEachMeasurementSeriesByExpr iterates over all series in a measurement filtered by an expression.
func (i *Index) ForEachMeasurementSeriesByExpr(name []byte, condition influxql.Expr, fn func(tags models.Tags) error) error {
	itr, err := i.MeasurementSeriesByExprIterator(name, condition)
	if err != nil {
		return err
	} else if itr == nil {
		return nil
	}

	for e := itr.Next(); e != nil; e = itr.Next() {
		if err := fn(e.Tags()); err != nil {
			return err
		}
	}

	return nil
}

// ForEachMeasurementTagKey iterates over all tag keys in a measurement.
func (i *Index) ForEachMeasurementTagKey(name []byte, fn func(key []byte) error) error {
	itr := i.TagKeyIterator(name)
	if itr == nil {
		return nil
	}

	for e := itr.Next(); e != nil; e = itr.Next() {
		if err := fn(e.Key()); err != nil {
			return err
		}
	}

	return nil
}

// TagSets returns an ordered list of tag sets for a measurement by dimension
// and filtered by an optional conditional expression.
func (i *Index) TagSets(name []byte, dimensions []string, condition influxql.Expr) ([]*influxql.TagSet, error) {
	itr, err := i.MeasurementSeriesByExprIterator(name, condition)
	if err != nil {
		return nil, err
	} else if itr == nil {
		return nil, nil
	}

	// For every series, get the tag values for the requested tag keys i.e.
	// dimensions. This is the TagSet for that series. Series with the same
	// TagSet are then grouped together, because for the purpose of GROUP BY
	// they are part of the same composite series.
	tagSets := make(map[string]*influxql.TagSet, 64)

	for e := itr.Next(); e != nil; e = itr.Next() {
		tags := make(map[string]string, len(dimensions))

		// Build the TagSet for this series.
		for _, dim := range dimensions {
			tags[dim] = e.Tags().GetString(dim)
		}

		// Convert the TagSet to a string, so it can be added to a map
		// allowing TagSets to be handled as a set.
		tagsAsKey := tsdb.MarshalTags(tags)
		tagSet, ok := tagSets[string(tagsAsKey)]
		if !ok {
			// This TagSet is new, create a new entry for it.
			tagSet = &influxql.TagSet{
				Tags: tags,
				Key:  tagsAsKey,
			}
		}
		// Associate the series and filter with the Tagset.
		tagSet.AddFilter(string(SeriesElemKey(e)), e.Expr())

		// Ensure it's back in the map.
		tagSets[string(tagsAsKey)] = tagSet
	}

	// Sort the series in each tag set.
	for _, t := range tagSets {
		sort.Sort(t)
	}

	// The TagSets have been created, as a map of TagSets. Just send
	// the values back as a slice, sorting for consistency.
	sortedTagsSets := make([]*influxql.TagSet, 0, len(tagSets))
	for _, v := range tagSets {
		sortedTagsSets = append(sortedTagsSets, v)
	}
	sort.Sort(byTagKey(sortedTagsSets))

	return sortedTagsSets, nil
}

// MeasurementSeriesByExprIterator returns a series iterator for a measurement
// that is filtered by expr. If expr only contains time expressions then this
// call is equivalent to MeasurementSeriesIterator().
func (i *Index) MeasurementSeriesByExprIterator(name []byte, expr influxql.Expr) (SeriesIterator, error) {
	// Return all series for the measurement if there are no tag expressions.
	if expr == nil || influxql.OnlyTimeExpr(expr) {
		return i.MeasurementSeriesIterator(name), nil
	}
	return i.seriesByExprIterator(name, expr, i.fieldset.CreateFieldsIfNotExists(string(name)))
}

// MeasurementSeriesKeysByExpr returns a list of series keys matching expr.
func (i *Index) MeasurementSeriesKeysByExpr(name []byte, expr influxql.Expr) ([][]byte, error) {
	// Create iterator for all matching series.
	itr, err := i.MeasurementSeriesByExprIterator(name, expr)
	if err != nil {
		return nil, err
	} else if itr == nil {
		return nil, nil
	}

	// Iterate over all series and generate keys.
	var keys [][]byte
	for e := itr.Next(); e != nil; e = itr.Next() {
		// Check for unsupported field filters.
		// Any remaining filters means there were fields (e.g., `WHERE value = 1.2`).
		if e.Expr() != nil {
			return nil, errors.New("fields not supported in WHERE clause during deletion")
		}

		keys = append(keys, models.MakeKey(e.Name(), e.Tags()))
	}
	return keys, nil
}

func (i *Index) seriesByExprIterator(name []byte, expr influxql.Expr, mf *tsdb.MeasurementFields) (SeriesIterator, error) {
	switch expr := expr.(type) {
	case *influxql.BinaryExpr:
		switch expr.Op {
		case influxql.AND, influxql.OR:
			// Get the series IDs and filter expressions for the LHS.
			litr, err := i.seriesByExprIterator(name, expr.LHS, mf)
			if err != nil {
				return nil, err
			}

			// Get the series IDs and filter expressions for the RHS.
			ritr, err := i.seriesByExprIterator(name, expr.RHS, mf)
			if err != nil {
				return nil, err
			}

			// Intersect iterators if expression is "AND".
			if expr.Op == influxql.AND {
				return IntersectSeriesIterators(litr, ritr), nil
			}

			// Union iterators if expression is "OR".
			return UnionSeriesIterators(litr, ritr), nil

		default:
			return i.seriesByBinaryExprIterator(name, expr, mf)
		}

	case *influxql.ParenExpr:
		return i.seriesByExprIterator(name, expr.Expr, mf)

	default:
		return nil, nil
	}
}

// seriesByBinaryExprIterator returns a series iterator and a filtering expression.
func (i *Index) seriesByBinaryExprIterator(name []byte, n *influxql.BinaryExpr, mf *tsdb.MeasurementFields) (SeriesIterator, error) {
	// If this binary expression has another binary expression, then this
	// is some expression math and we should just pass it to the underlying query.
	if _, ok := n.LHS.(*influxql.BinaryExpr); ok {
		return newSeriesExprIterator(i.MeasurementSeriesIterator(name), n), nil
	} else if _, ok := n.RHS.(*influxql.BinaryExpr); ok {
		return newSeriesExprIterator(i.MeasurementSeriesIterator(name), n), nil
	}

	// Retrieve the variable reference from the correct side of the expression.
	key, ok := n.LHS.(*influxql.VarRef)
	value := n.RHS
	if !ok {
		key, ok = n.RHS.(*influxql.VarRef)
		if !ok {
			return nil, fmt.Errorf("invalid expression: %s", n.String())
		}
		value = n.LHS
	}

	// For time literals, return all series and "true" as the filter.
	if _, ok := value.(*influxql.TimeLiteral); ok || key.Val == "time" {
		return newSeriesExprIterator(i.MeasurementSeriesIterator(name), &influxql.BooleanLiteral{Val: true}), nil
	}

	// For fields, return all series from this measurement.
	if key.Val != "_name" && ((key.Type == influxql.Unknown && mf.HasField(key.Val)) || key.Type == influxql.AnyField || (key.Type != influxql.Tag && key.Type != influxql.Unknown)) {
		return newSeriesExprIterator(i.MeasurementSeriesIterator(name), n), nil
	} else if value, ok := value.(*influxql.VarRef); ok {
		// Check if the RHS is a variable and if it is a field.
		if value.Val != "_name" && ((value.Type == influxql.Unknown && mf.HasField(value.Val)) || key.Type == influxql.AnyField || (value.Type != influxql.Tag && value.Type != influxql.Unknown)) {
			return newSeriesExprIterator(i.MeasurementSeriesIterator(name), n), nil
		}
	}

	// Create iterator based on value type.
	switch value := value.(type) {
	case *influxql.StringLiteral:
		return i.seriesByBinaryExprStringIterator(name, []byte(key.Val), []byte(value.Val), n.Op)
	case *influxql.RegexLiteral:
		return i.seriesByBinaryExprRegexIterator(name, []byte(key.Val), value.Val, n.Op)
	case *influxql.VarRef:
		return i.seriesByBinaryExprVarRefIterator(name, []byte(key.Val), value, n.Op)
	default:
		if n.Op == influxql.NEQ || n.Op == influxql.NEQREGEX {
			return i.MeasurementSeriesIterator(name), nil
		}
		return nil, nil
	}
}

func (i *Index) seriesByBinaryExprStringIterator(name, key, value []byte, op influxql.Token) (SeriesIterator, error) {
	// Special handling for "_name" to match measurement name.
	if bytes.Equal(key, []byte("_name")) {
		if (op == influxql.EQ && bytes.Equal(value, name)) || (op == influxql.NEQ && !bytes.Equal(value, name)) {
			return i.MeasurementSeriesIterator(name), nil
		}
		return nil, nil
	}

	if op == influxql.EQ {
		// Match a specific value.
		if len(value) != 0 {
			return i.TagValueSeriesIterator(name, key, value), nil
		}

		// Return all measurement series that have no values from this tag key.
		return DifferenceSeriesIterators(
			i.MeasurementSeriesIterator(name),
			i.TagKeySeriesIterator(name, key),
		), nil
	}

	// Return all measurement series without this tag value.
	if len(value) != 0 {
		return DifferenceSeriesIterators(
			i.MeasurementSeriesIterator(name),
			i.TagValueSeriesIterator(name, key, value),
		), nil
	}

	// Return all series across all values of this tag key.
	return i.TagKeySeriesIterator(name, key), nil
}

func (i *Index) seriesByBinaryExprRegexIterator(name, key []byte, value *regexp.Regexp, op influxql.Token) (SeriesIterator, error) {
	// Special handling for "_name" to match measurement name.
	if bytes.Equal(key, []byte("_name")) {
		match := value.Match(name)
		if (op == influxql.EQREGEX && match) || (op == influxql.NEQREGEX && !match) {
			return newSeriesExprIterator(i.MeasurementSeriesIterator(name), &influxql.BooleanLiteral{Val: true}), nil
		}
		return nil, nil
	}
	return i.MatchTagValueSeriesIterator(name, key, value, op == influxql.EQREGEX), nil
}

func (i *Index) seriesByBinaryExprVarRefIterator(name, key []byte, value *influxql.VarRef, op influxql.Token) (SeriesIterator, error) {
	if op == influxql.EQ {
		return IntersectSeriesIterators(
			i.TagKeySeriesIterator(name, key),
			i.TagKeySeriesIterator(name, []byte(value.Val)),
		), nil
	}

	return DifferenceSeriesIterators(
		i.TagKeySeriesIterator(name, key),
		i.TagKeySeriesIterator(name, []byte(value.Val)),
	), nil
}

func (i *Index) SetFieldName(measurement, name string)  {}
func (i *Index) RemoveShard(shardID uint64)             {}
func (i *Index) AssignShard(k string, shardID uint64)   {}
func (i *Index) UnassignShard(k string, shardID uint64) {}

// SeriesPointIterator returns an influxql iterator over all series.
func (i *Index) SeriesPointIterator(opt influxql.IteratorOptions) (influxql.Iterator, error) {
	return newSeriesPointIterator(i, opt), nil
}

// File represents a log or index file.
type File interface {
	Measurement(name []byte) MeasurementElem
	Series(name []byte, tags models.Tags) SeriesElem

	TagKeyIterator(name []byte) TagKeyIterator

	TagValue(name, key, value []byte) TagValueElem
	TagValueIterator(name, key []byte) TagValueIterator

	// Series iteration.
	SeriesIterator() SeriesIterator
	MeasurementSeriesIterator(name []byte) SeriesIterator
	TagKeySeriesIterator(name, key []byte) SeriesIterator
	TagValueSeriesIterator(name, key, value []byte) SeriesIterator
}

// FilterExprs represents a map of series IDs to filter expressions.
type FilterExprs map[uint64]influxql.Expr

// DeleteBoolLiteralTrues deletes all elements whose filter expression is a boolean literal true.
func (fe FilterExprs) DeleteBoolLiteralTrues() {
	for id, expr := range fe {
		if e, ok := expr.(*influxql.BooleanLiteral); ok && e.Val == true {
			delete(fe, id)
		}
	}
}

// Len returns the number of elements.
func (fe FilterExprs) Len() int {
	if fe == nil {
		return 0
	}
	return len(fe)
}

// seriesPointIterator adapts SeriesIterator to an influxql.Iterator.
type seriesPointIterator struct {
	index *Index
	mitr  MeasurementIterator
	sitr  SeriesIterator
	opt   influxql.IteratorOptions

	point influxql.FloatPoint // reusable point
}

// newSeriesPointIterator returns a new instance of seriesPointIterator.
func newSeriesPointIterator(index *Index, opt influxql.IteratorOptions) *seriesPointIterator {
	return &seriesPointIterator{
		index: index,
		mitr:  index.MeasurementIterator(),
		point: influxql.FloatPoint{
			Aux: make([]interface{}, len(opt.Aux)),
		},
		opt: opt,
	}
}

// Stats returns stats about the points processed.
func (itr *seriesPointIterator) Stats() influxql.IteratorStats { return influxql.IteratorStats{} }

// Close closes the iterator.
func (itr *seriesPointIterator) Close() error { return nil }

// Next emits the next point in the iterator.
func (itr *seriesPointIterator) Next() (*influxql.FloatPoint, error) {
	for {
		// Create new series iterator, if necessary.
		// Exit if there are no measurements remaining.
		if itr.sitr == nil {
			m := itr.mitr.Next()
			if m == nil {
				return nil, nil
			}

			sitr, err := itr.index.MeasurementSeriesByExprIterator(m.Name(), itr.opt.Condition)
			if err != nil {
				return nil, err
			} else if sitr == nil {
				continue
			}
			itr.sitr = sitr
		}

		// Read next series element.
		e := itr.sitr.Next()
		if e == nil {
			itr.sitr = nil
			continue
		}

		// Convert to a key.
		key := string(models.MakeKey(e.Name(), e.Tags()))

		// Write auxiliary fields.
		for i, f := range itr.opt.Aux {
			switch f.Val {
			case "key":
				itr.point.Aux[i] = key
			}
		}
		return &itr.point, nil
	}
}

// unionStringSets returns the union of two sets
func unionStringSets(a, b map[string]struct{}) map[string]struct{} {
	other := make(map[string]struct{})
	for k := range a {
		other[k] = struct{}{}
	}
	for k := range b {
		other[k] = struct{}{}
	}
	return other
}

// intersectStringSets returns the intersection of two sets.
func intersectStringSets(a, b map[string]struct{}) map[string]struct{} {
	if len(a) < len(b) {
		a, b = b, a
	}

	other := make(map[string]struct{})
	for k := range a {
		if _, ok := b[k]; ok {
			other[k] = struct{}{}
		}
	}
	return other
}

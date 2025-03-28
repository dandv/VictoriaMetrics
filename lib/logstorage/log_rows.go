package logstorage

import (
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

// LogRows holds a set of rows needed for Storage.MustAddRows
//
// LogRows must be obtained via GetLogRows()
type LogRows struct {
	// a holds all the bytes referred by items in LogRows
	a arena

	// fieldsBuf holds all the fields referred by items in LogRows
	fieldsBuf []Field

	// streamIDs holds streamIDs for rows added to LogRows
	streamIDs []streamID

	// streamTagsCanonicals holds streamTagsCanonical entries for rows added to LogRows
	streamTagsCanonicals []string

	// timestamps holds stimestamps for rows added to LogRows
	timestamps []int64

	// rows holds fields for rows added to LogRows.
	rows [][]Field

	// sf is a helper for sorting fields in every added row
	sf sortedFields

	// streamFields contains names for stream fields
	streamFields map[string]struct{}

	// ignoreFields is a filter for fields, which must be ignored during data ingestion
	ignoreFields fieldsFilter

	// extraFields contains extra fields to add to all the logs at MustAdd().
	extraFields []Field

	// extraStreamFields contains extraFields, which must be treated as stream fields.
	extraStreamFields []Field

	// defaultMsgValue contains default value for missing _msg field
	defaultMsgValue string
}

type sortedFields []Field

func (sf *sortedFields) Len() int {
	return len(*sf)
}

func (sf *sortedFields) Less(i, j int) bool {
	a := *sf
	return a[i].Name < a[j].Name
}

func (sf *sortedFields) Swap(i, j int) {
	a := *sf
	a[i], a[j] = a[j], a[i]
}

// Reset resets lr with all its settings.
//
// Call ResetKeepSettings() for resetting lr without resetting its settings.
func (lr *LogRows) Reset() {
	lr.ResetKeepSettings()

	sfs := lr.streamFields
	for k := range sfs {
		delete(sfs, k)
	}

	lr.ignoreFields.reset()

	lr.extraFields = nil

	clear(lr.extraStreamFields)
	lr.extraStreamFields = lr.extraStreamFields[:0]

	lr.defaultMsgValue = ""
}

// ResetKeepSettings resets rows stored in lr, while keeping its settings passed to GetLogRows().
func (lr *LogRows) ResetKeepSettings() {
	lr.a.reset()

	fb := lr.fieldsBuf
	for i := range fb {
		fb[i].Reset()
	}
	lr.fieldsBuf = fb[:0]

	sids := lr.streamIDs
	for i := range sids {
		sids[i].reset()
	}
	lr.streamIDs = sids[:0]

	clear(lr.streamTagsCanonicals)
	lr.streamTagsCanonicals = lr.streamTagsCanonicals[:0]

	lr.timestamps = lr.timestamps[:0]

	clear(lr.rows)
	lr.rows = lr.rows[:0]

	lr.sf = nil
}

// NeedFlush returns true if lr contains too much data, so it must be flushed to the storage.
func (lr *LogRows) NeedFlush() bool {
	return len(lr.a.b) > (maxUncompressedBlockSize/8)*7
}

// MustAdd adds a log entry with the given args to lr.
//
// If streamFields is non-nil, the the given streamFields are used as log stream fields
// instead of the pre-configured stream fields from GetLogRows().
//
// It is OK to modify the args after returning from the function,
// since lr copies all the args to internal data.
//
// Log entries are dropped with the warning message in the following cases:
// - if there are too many log fields
// - if there are too long log field names
// - if the total length of log entries is too long
func (lr *LogRows) MustAdd(tenantID TenantID, timestamp int64, fields, streamFields []Field) {
	// Verify that the log entry doesn't exceed limits.
	if len(fields) > maxColumnsPerBlock {
		line := MarshalFieldsToJSON(nil, fields)
		logger.Warnf("ignoring log entry with too big number of fields %d, since it exceeds the limit %d; "+
			"see https://docs.victoriametrics.com/victorialogs/faq/#how-many-fields-a-single-log-entry-may-contain ; log entry: %s", len(fields), maxColumnsPerBlock, line)
		return
	}
	for i := range fields {
		fieldName := fields[i].Name
		if len(fieldName) > maxFieldNameSize {
			line := MarshalFieldsToJSON(nil, fields)
			logger.Warnf("ignoring log entry with too long field name %q, since its length (%d) exceeds the limit %d bytes; "+
				"see https://docs.victoriametrics.com/victorialogs/faq/#what-is-the-maximum-supported-field-name-length ; log entry: %s",
				fieldName, len(fieldName), maxFieldNameSize, line)
			return
		}
	}
	rowLen := uncompressedRowSizeBytes(fields)
	if rowLen > maxUncompressedBlockSize {
		line := MarshalFieldsToJSON(nil, fields)
		logger.Warnf("ignoring too long log entry with the estimated length of %d bytes, since it exceeds the limit %d bytes; "+
			"see https://docs.victoriametrics.com/victorialogs/faq/#what-length-a-log-record-is-expected-to-have ; log entry: %s", rowLen, maxUncompressedBlockSize, line)
		return
	}

	// Compose StreamTags from fields according to streamFields, lr.streamFields and lr.extraStreamFields
	st := GetStreamTags()
	if streamFields != nil {
		// streamFields override lr.streamFields
		for _, f := range streamFields {
			if !lr.ignoreFields.match(f.Name) {
				st.Add(f.Name, f.Value)
			}
		}
	} else {
		for _, f := range fields {
			if _, ok := lr.streamFields[f.Name]; ok {
				st.Add(f.Name, f.Value)
			}
		}
		for _, f := range lr.extraStreamFields {
			st.Add(f.Name, f.Value)
		}
	}

	// Marshal StreamTags
	bb := bbPool.Get()
	bb.B = st.MarshalCanonical(bb.B)
	PutStreamTags(st)

	// Calculate the id for the StreamTags
	var sid streamID
	sid.tenantID = tenantID
	sid.id = hash128(bb.B)

	// Store the row
	streamTagsCanonical := bytesutil.ToUnsafeString(bb.B)
	lr.mustAddInternal(sid, timestamp, fields, streamTagsCanonical)
	bbPool.Put(bb)
}

func (lr *LogRows) mustAddInternal(sid streamID, timestamp int64, fields []Field, streamTagsCanonical string) {
	stcs := lr.streamTagsCanonicals
	if len(stcs) > 0 && string(stcs[len(stcs)-1]) == streamTagsCanonical {
		stcs = append(stcs, stcs[len(stcs)-1])
	} else {
		streamTagsCanonicalCopy := lr.a.copyString(streamTagsCanonical)
		stcs = append(stcs, streamTagsCanonicalCopy)
	}
	lr.streamTagsCanonicals = stcs

	lr.streamIDs = append(lr.streamIDs, sid)
	lr.timestamps = append(lr.timestamps, timestamp)

	fieldsLen := len(lr.fieldsBuf)
	hasMsgField := lr.addFieldsInternal(fields, &lr.ignoreFields, true)
	if lr.addFieldsInternal(lr.extraFields, nil, false) {
		hasMsgField = true
	}

	// Add optional default _msg field
	if !hasMsgField && lr.defaultMsgValue != "" {
		lr.fieldsBuf = append(lr.fieldsBuf, Field{
			Value: lr.defaultMsgValue,
		})
	}

	// Add log row fields to lr.rows
	row := lr.fieldsBuf[fieldsLen:]
	lr.rows = append(lr.rows, row)
}

func (lr *LogRows) addFieldsInternal(fields []Field, ignoreFields *fieldsFilter, mustCopyFields bool) bool {
	if len(fields) == 0 {
		return false
	}

	var prevRow []Field
	if len(lr.rows) > 0 {
		prevRow = lr.rows[len(lr.rows)-1]
	}

	fb := lr.fieldsBuf
	hasMsgField := false
	for i := range fields {
		f := &fields[i]

		if ignoreFields.match(f.Name) {
			continue
		}
		if f.Value == "" {
			// Skip fields without values
			continue
		}

		var prevField *Field
		if prevRow != nil && i < len(prevRow) {
			prevField = &prevRow[i]
		}

		fb = append(fb, Field{})
		dstField := &fb[len(fb)-1]

		fieldName := f.Name
		if fieldName == "_msg" {
			fieldName = ""
			hasMsgField = true
		}

		if prevField != nil && prevField.Name == fieldName {
			dstField.Name = prevField.Name
		} else {
			if mustCopyFields {
				dstField.Name = lr.a.copyString(fieldName)
			} else {
				dstField.Name = fieldName
			}
			prevRow = nil
		}
		if prevField != nil && prevField.Value == f.Value {
			dstField.Value = prevField.Value
		} else {
			if mustCopyFields {
				dstField.Value = lr.a.copyString(f.Value)
			} else {
				dstField.Value = f.Value
			}
		}
	}
	lr.fieldsBuf = fb

	return hasMsgField
}

func (lr *LogRows) sortFieldsInRows() {
	for _, row := range lr.rows {
		lr.sf = row
		sort.Sort(&lr.sf)
	}
}

// GetRowString returns string representation of the row with the given idx.
func (lr *LogRows) GetRowString(idx int) string {
	tf := TimeFormatter(lr.timestamps[idx])
	streamTags := getStreamTagsString(lr.streamTagsCanonicals[idx])
	var fields []Field
	fields = append(fields[:0], lr.rows[idx]...)
	fields = append(fields, Field{
		Name:  "_time",
		Value: tf.String(),
	})
	fields = append(fields, Field{
		Name:  "_stream",
		Value: streamTags,
	})
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].Name < fields[j].Name
	})
	line := MarshalFieldsToJSON(nil, fields)
	return string(line)
}

// GetLogRows returns LogRows from the pool for the given streamFields.
//
// streamFields is a set of field names, which must be associated with the stream.
//
// ignoreFields is a set of field names, which must be ignored during data ingestion.
// ignoreFields entries may end with `*`. In this case they match any fields with the prefix until '*'.
//
// extraFields is a set of fields, which must be added to all the logs passed to MustAdd().
//
// defaultMsgValue is the default value to store in non-existing or empty _msg.
//
// Return back it to the pool with PutLogRows() when it is no longer needed.
func GetLogRows(streamFields, ignoreFields []string, extraFields []Field, defaultMsgValue string) *LogRows {
	v := logRowsPool.Get()
	if v == nil {
		v = &LogRows{}
	}
	lr := v.(*LogRows)

	// initialize ignoreFields
	lr.ignoreFields.addMulti(ignoreFields)
	for _, f := range extraFields {
		// Extra fields must override the existing fields for the sake of consistency and security,
		// so the client won't be able to override them.
		lr.ignoreFields.add(f.Name)
	}

	// Initialize streamFields
	sfs := lr.streamFields
	if sfs == nil {
		sfs = make(map[string]struct{}, len(streamFields))
		lr.streamFields = sfs
	}
	for _, f := range streamFields {
		if !lr.ignoreFields.match(f) {
			sfs[f] = struct{}{}
		}
	}

	// Initialize extraStreamFields
	for _, f := range extraFields {
		if slices.Contains(streamFields, f.Name) {
			lr.extraStreamFields = append(lr.extraStreamFields, f)
			delete(sfs, f.Name)
		}
	}

	lr.extraFields = extraFields
	lr.defaultMsgValue = defaultMsgValue

	return lr
}

// PutLogRows returns lr to the pool.
func PutLogRows(lr *LogRows) {
	lr.Reset()
	logRowsPool.Put(lr)
}

var logRowsPool sync.Pool

// Len returns the number of items in lr.
func (lr *LogRows) Len() int {
	return len(lr.streamIDs)
}

// Less returns true if (streamID, timestamp) for row i is smaller than the (streamID, timestamp) for row j
func (lr *LogRows) Less(i, j int) bool {
	a := &lr.streamIDs[i]
	b := &lr.streamIDs[j]
	if !a.equal(b) {
		return a.less(b)
	}
	return lr.timestamps[i] < lr.timestamps[j]
}

// Swap swaps rows i and j in lr.
func (lr *LogRows) Swap(i, j int) {
	a := &lr.streamIDs[i]
	b := &lr.streamIDs[j]
	*a, *b = *b, *a

	tsA, tsB := &lr.timestamps[i], &lr.timestamps[j]
	*tsA, *tsB = *tsB, *tsA

	snA, snB := &lr.streamTagsCanonicals[i], &lr.streamTagsCanonicals[j]
	*snA, *snB = *snB, *snA

	fieldsA, fieldsB := &lr.rows[i], &lr.rows[j]
	*fieldsA, *fieldsB = *fieldsB, *fieldsA
}

// EstimatedJSONRowLen returns an approximate length of the log entry with the given fields if represented as JSON.
func EstimatedJSONRowLen(fields []Field) int {
	n := len("{}\n")
	n += len(`"_time":""`) + len(time.RFC3339Nano)
	for _, f := range fields {
		nameLen := len(f.Name)
		if nameLen == 0 {
			nameLen = len("_msg")
		}
		n += len(`,"":""`) + nameLen + len(f.Value)
	}
	return n
}

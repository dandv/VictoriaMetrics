package journald

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vlinsert/insertutils"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vlstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/protoparser/protoparserutil"
	"github.com/VictoriaMetrics/metrics"
)

// See https://github.com/systemd/systemd/blob/main/src/libsystemd/sd-journal/journal-file.c#L1703
const journaldEntryMaxNameLen = 64

var allowedJournaldEntryNameChars = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*`)

var (
	journaldStreamFields = flagutil.NewArrayString("journald.streamFields", "Comma-separated list of fields to use as log stream fields for logs ingested over journald protocol. "+
		"See https://docs.victoriametrics.com/victorialogs/data-ingestion/journald/#stream-fields")
	journaldIgnoreFields = flagutil.NewArrayString("journald.ignoreFields", "Comma-separated list of fields to ignore for logs ingested over journald protocol. "+
		"See https://docs.victoriametrics.com/victorialogs/data-ingestion/journald/#dropping-fields")
	journaldTimeField = flag.String("journald.timeField", "__REALTIME_TIMESTAMP", "Field to use as a log timestamp for logs ingested via journald protocol. "+
		"See https://docs.victoriametrics.com/victorialogs/data-ingestion/journald/#time-field")
	journaldTenantID = flag.String("journald.tenantID", "0:0", "TenantID for logs ingested via the Journald endpoint. "+
		"See https://docs.victoriametrics.com/victorialogs/data-ingestion/journald/#multitenancy")
	journaldIncludeEntryMetadata = flag.Bool("journald.includeEntryMetadata", false, "Include journal entry fields, which with double underscores.")

	maxRequestSize = flagutil.NewBytes("journald.maxRequestSize", 64*1024*1024, "The maximum size in bytes of a single journald request")
)

func getCommonParams(r *http.Request) (*insertutils.CommonParams, error) {
	cp, err := insertutils.GetCommonParams(r)
	if err != nil {
		return nil, err
	}
	if cp.TenantID.AccountID == 0 && cp.TenantID.ProjectID == 0 {
		tenantID, err := logstorage.ParseTenantID(*journaldTenantID)
		if err != nil {
			return nil, fmt.Errorf("cannot parse -journald.tenantID=%q for journald: %w", *journaldTenantID, err)
		}
		cp.TenantID = tenantID
	}
	if cp.TimeField != "" {
		cp.TimeField = *journaldTimeField
	}
	if len(cp.StreamFields) == 0 {
		cp.StreamFields = *journaldStreamFields
	}
	if len(cp.IgnoreFields) == 0 {
		cp.IgnoreFields = *journaldIgnoreFields
	}
	cp.MsgFields = []string{"MESSAGE"}
	return cp, nil
}

// RequestHandler processes Journald Export insert requests
func RequestHandler(path string, w http.ResponseWriter, r *http.Request) bool {
	switch path {
	case "/upload":
		if r.Header.Get("Content-Type") != "application/vnd.fdo.journal" {
			httpserver.Errorf(w, r, "only application/vnd.fdo.journal encoding is supported for Journald")
			return true
		}
		handleJournald(r, w)
		return true
	default:
		return false
	}
}

// handleJournald parses Journal binary entries
func handleJournald(r *http.Request, w http.ResponseWriter) {
	startTime := time.Now()
	requestsJournaldTotal.Inc()

	cp, err := getCommonParams(r)
	if err != nil {
		errorsTotal.Inc()
		httpserver.Errorf(w, r, "cannot parse common params from request: %s", err)
		return
	}

	if err := vlstorage.CanWriteData(); err != nil {
		errorsTotal.Inc()
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	encoding := r.Header.Get("Content-Encoding")
	err = protoparserutil.ReadUncompressedData(r.Body, encoding, maxRequestSize, func(data []byte) error {
		lmp := cp.NewLogMessageProcessor("journald", false)
		err := parseJournaldRequest(data, lmp, cp)
		lmp.MustClose()
		return err
	})
	if err != nil {
		errorsTotal.Inc()
		httpserver.Errorf(w, r, "cannot read journald protocol data: %s", err)
		return
	}

	// systemd starting release v258 will support compression, which starts working after negotiation: it expects supported compression
	// algorithms list in Accept-Encoding response header in a format "<algorithm_1>[:<priority_1>][;<algorithm_2>:<priority_2>]"
	// See https://github.com/systemd/systemd/pull/34822
	w.Header().Set("Accept-Encoding", "zstd")

	// update requestJournaldDuration only for successfully parsed requests
	// There is no need in updating requestJournaldDuration for request errors,
	// since their timings are usually much smaller than the timing for successful request parsing.
	requestJournaldDuration.UpdateDuration(startTime)
}

var (
	requestsJournaldTotal = metrics.NewCounter(`vl_http_requests_total{path="/insert/journald/upload"}`)
	errorsTotal           = metrics.NewCounter(`vl_http_errors_total{path="/insert/journald/upload"}`)

	requestJournaldDuration = metrics.NewHistogram(`vl_http_request_duration_seconds{path="/insert/journald/upload"}`)
)

// See https://systemd.io/JOURNAL_EXPORT_FORMATS/#journal-export-format
func parseJournaldRequest(data []byte, lmp insertutils.LogMessageProcessor, cp *insertutils.CommonParams) error {
	var fields []logstorage.Field
	var ts int64
	var size uint64
	var name, value string
	var line []byte

	currentTimestamp := time.Now().UnixNano()

	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		switch {
		case idx > 0:
			// process fields
			line = data[:idx]
			data = data[idx+1:]
		case idx == 0:
			// next message or end of file
			// double new line is a separator for the next message
			if len(fields) > 0 {
				if ts == 0 {
					ts = currentTimestamp
				}
				lmp.AddRow(ts, fields, nil)
				fields = fields[:0]
			}
			// skip newline separator
			data = data[1:]
			continue
		case idx < 0:
			return fmt.Errorf("missing new line separator, unread data left=%d", len(data))
		}

		idx = bytes.IndexByte(line, '=')
		// could b either e key=value\n pair
		// or just  key\n
		// with binary data at the buffer
		if idx > 0 {
			name = bytesutil.ToUnsafeString(line[:idx])
			value = bytesutil.ToUnsafeString(line[idx+1:])
		} else {
			name = bytesutil.ToUnsafeString(line)
			if len(data) == 0 {
				return fmt.Errorf("unexpected zero data for binary field value of key=%s", name)
			}
			// size of binary data encoded as le i64 at the begging
			idx, err := binary.Decode(data, binary.LittleEndian, &size)
			if err != nil {
				return fmt.Errorf("failed to extract binary field %q value size: %w", name, err)
			}
			// skip binary data size
			data = data[idx:]
			if size == 0 {
				return fmt.Errorf("unexpected zero binary data size decoded %d", size)
			}
			if int(size) > len(data) {
				return fmt.Errorf("binary data size=%d cannot exceed size of the data at buffer=%d", size, len(data))
			}
			value = bytesutil.ToUnsafeString(data[:size])
			data = data[int(size):]
			// binary data must has new line separator for the new line or next field
			if len(data) == 0 {
				return fmt.Errorf("unexpected empty buffer after binary field=%s read", name)
			}
			lastB := data[0]
			if lastB != '\n' {
				return fmt.Errorf("expected new line separator after binary field=%s, got=%s", name, string(lastB))
			}
			data = data[1:]
		}
		if len(name) > journaldEntryMaxNameLen {
			return fmt.Errorf("journald entry name should not exceed %d symbols, got: %q", journaldEntryMaxNameLen, name)
		}
		if !allowedJournaldEntryNameChars.MatchString(name) {
			return fmt.Errorf("journald entry name should consist of `A-Z0-9_` characters and must start from non-digit symbol")
		}
		if name == cp.TimeField {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse Journald timestamp, %w", err)
			}
			ts = n * 1e3
			continue
		}

		if slices.Contains(cp.MsgFields, name) {
			name = "_msg"
		}

		if *journaldIncludeEntryMetadata || !strings.HasPrefix(name, "__") {
			fields = append(fields, logstorage.Field{
				Name:  name,
				Value: value,
			})
		}
	}
	if len(fields) > 0 {
		if ts == 0 {
			ts = currentTimestamp
		}
		lmp.AddRow(ts, fields, nil)
	}
	return nil
}

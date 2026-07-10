package osm

import (
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
)

// Change is one parsed osmChange (.osc) document. Creates and modifies are
// merged: both mean "this is the object's new state".
type Change struct {
	NodeUpsert []NodeChange
	NodeDelete []int64
	WayUpsert  []WayChange
	WayDelete  []int64
}

type NodeChange struct {
	ID       int64
	Lat, Lon int32 // E7
}

type WayChange struct {
	ID   int64
	Refs []int64
	Tags map[string]string
}

// ParseOSCGz reads a gzip-compressed osmChange stream.
func ParseOSCGz(r io.Reader) (*Change, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("osc: %w", err)
	}
	defer gz.Close()
	return ParseOSC(gz)
}

// ParseOSC reads an osmChange XML stream.
func ParseOSC(r io.Reader) (*Change, error) {
	ch := &Change{}
	dec := xml.NewDecoder(r)
	mode := "" // create | modify | delete
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return ch, nil
		}
		if err != nil {
			return nil, fmt.Errorf("osc: %w", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "create", "modify", "delete":
				mode = el.Name.Local
			case "node":
				id, lat, lon := int64(0), 0.0, 0.0
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "id":
						id, _ = strconv.ParseInt(a.Value, 10, 64)
					case "lat":
						lat, _ = strconv.ParseFloat(a.Value, 64)
					case "lon":
						lon, _ = strconv.ParseFloat(a.Value, 64)
					}
				}
				if id == 0 {
					dec.Skip()
					continue
				}
				if mode == "delete" {
					ch.NodeDelete = append(ch.NodeDelete, id)
				} else {
					ch.NodeUpsert = append(ch.NodeUpsert, NodeChange{
						ID: id, Lat: int32(lat * 1e7), Lon: int32(lon * 1e7),
					})
				}
				dec.Skip() // node tags don't affect routing (yet)
			case "way":
				var id int64
				for _, a := range el.Attr {
					if a.Name.Local == "id" {
						id, _ = strconv.ParseInt(a.Value, 10, 64)
					}
				}
				if id == 0 {
					dec.Skip()
					continue
				}
				if mode == "delete" {
					ch.WayDelete = append(ch.WayDelete, id)
					dec.Skip()
					continue
				}
				wc := WayChange{ID: id, Tags: map[string]string{}}
				if err := parseWayBody(dec, &wc); err != nil {
					return nil, err
				}
				ch.WayUpsert = append(ch.WayUpsert, wc)
			case "relation":
				dec.Skip()
			}
		case xml.EndElement:
			if el.Name.Local == "create" || el.Name.Local == "modify" || el.Name.Local == "delete" {
				mode = ""
			}
		}
	}
}

func parseWayBody(dec *xml.Decoder, wc *WayChange) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("osc: way %d: %w", wc.ID, err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "nd":
				for _, a := range el.Attr {
					if a.Name.Local == "ref" {
						ref, _ := strconv.ParseInt(a.Value, 10, 64)
						wc.Refs = append(wc.Refs, ref)
					}
				}
				dec.Skip()
			case "tag":
				var k, v string
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "k":
						k = a.Value
					case "v":
						v = a.Value
					}
				}
				if k != "" {
					wc.Tags[k] = v
				}
				dec.Skip()
			default:
				dec.Skip()
			}
		case xml.EndElement:
			if el.Name.Local == "way" {
				return nil
			}
		}
	}
}

// ReplicationState is a parsed osmosis state.txt.
type ReplicationState struct {
	Sequence  int64
	Timestamp string
}

// ParseStateTxt extracts sequenceNumber and timestamp.
func ParseStateTxt(data []byte) (ReplicationState, error) {
	st := ReplicationState{Sequence: -1}
	line := make([]byte, 0, 64)
	flush := func() {
		s := string(line)
		if len(s) > 15 && s[:15] == "sequenceNumber=" {
			st.Sequence, _ = strconv.ParseInt(s[15:], 10, 64)
		}
		if len(s) > 10 && s[:10] == "timestamp=" {
			st.Timestamp = s[10:]
		}
		line = line[:0]
	}
	for _, c := range data {
		if c == '\n' {
			flush()
			continue
		}
		if c != '\r' {
			line = append(line, c)
		}
	}
	flush()
	if st.Sequence < 0 {
		return st, fmt.Errorf("state.txt: no sequenceNumber found")
	}
	return st, nil
}

// SeqPath formats a replication sequence as the osmosis directory layout,
// e.g. 3901 → "000/003/901".
func SeqPath(seq int64) string {
	s := fmt.Sprintf("%09d", seq)
	return s[0:3] + "/" + s[3:6] + "/" + s[6:9]
}

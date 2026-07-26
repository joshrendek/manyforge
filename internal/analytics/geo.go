package analytics

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"

	"github.com/oschwald/maxminddb-golang/v2"
)

// MMDBResolver resolves countries from a MaxMind-format database.
type MMDBResolver struct {
	db *maxminddb.Reader
}

// OpenMMDB opens a MaxMind country database. An empty path or a missing configured file returns
// (nil, nil) because local and uncredentialed image builds intentionally omit GeoLite2; the missing
// file logs at slog.Warn level when logger is non-nil. Other open errors remain fatal so an
// unreadable or invalid database cannot make a broken deployment look healthy.
func OpenMMDB(path string, logger *slog.Logger) (*MMDBResolver, error) {
	if path == "" {
		return nil, nil
	}
	db, err := maxminddb.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if logger != nil {
				logger.Warn("analytics geoip database not found; country lookup disabled", "path", path)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("analytics: open geoip db %q: %w", path, err)
	}
	if logger != nil {
		logger.Info("analytics geoip database loaded", "path", path)
	}
	return &MMDBResolver{db: db}, nil
}

// Country returns the ISO 3166-1 alpha-2 code for ip, or "" when it cannot be placed or the
// resolver is nil/uninitialized.
//
// A lookup miss is silent by design: this runs on every pageview, so logging unresolvable
// addresses would produce a log line per request AND write client IPs into the logs — exactly the
// data the rest of this package works to never retain.
func (r *MMDBResolver) Country(ip net.IP) string {
	if r == nil || r.db == nil {
		return ""
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return ""
	}
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := r.db.Lookup(addr.Unmap()).Decode(&rec); err != nil {
		return ""
	}
	return rec.Country.ISOCode
}

// Close releases the database handle.
func (r *MMDBResolver) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

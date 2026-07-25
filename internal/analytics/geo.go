package analytics

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Country lookup is OPTIONAL and deliberately unbundled.
//
// A country breakdown needs an IP-to-country database, and every usable one carries licensing
// terms (MaxMind's GeoLite2 requires accepting an EULA and an account). Vendoring that data into
// the repo would make every deployment inherit those terms whether or not it wants the feature,
// and the file goes stale monthly, so a checked-in copy is quietly wrong within a quarter.
//
// So: the deployment points MANYFORGE_GEOIP_DB at a .mmdb file if it wants countries, and gets
// nothing if it does not. An absent breakdown is honest; a guessed one is worse than none.

// MMDBResolver resolves countries from a MaxMind-format database.
type MMDBResolver struct {
	db *maxminddb.Reader
	// mu guards nothing today but documents that db is read-only after construction; maxminddb
	// readers are safe for concurrent use.
	mu sync.RWMutex
}

// OpenMMDB opens a MaxMind country database. An empty path returns (nil, nil) — "no geo
// configured" is a normal state, not an error, and must not prevent the server from booting.
func OpenMMDB(path string, logger *slog.Logger) (*MMDBResolver, error) {
	if path == "" {
		return nil, nil
	}
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("analytics: open geoip db %q: %w", path, err)
	}
	if logger != nil {
		logger.Info("analytics geoip database loaded", "path", path)
	}
	return &MMDBResolver{db: db}, nil
}

// Country returns the ISO 3166-1 alpha-2 code for ip, or "" when it cannot be placed.
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.db.Close()
}

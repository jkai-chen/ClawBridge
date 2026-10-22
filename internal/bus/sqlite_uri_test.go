package bus

import (
	"net/url"
	"testing"
)

// A Windows path must reach SQLite as file:///C:/...; given raw it became
// the URI authority (file://C:%5C...) and every local open failed.
func TestSQLiteURIOpensWindowsAndPOSIXPaths(t *testing.T) {
	for _, c := range []struct{ goos, path, want string }{
		{"windows", `C:\Users\runneradmin\AppData\Local\Temp\x\bus.db`, "file:///C:/Users/runneradmin/AppData/Local/Temp/x/bus.db"},
		{"windows", `D:\a dir\#1?\bus.db`, "file:///D:/a%20dir/%231%3F/bus.db"},
		{"windows", `\\server\share\bus.db`, "file:////server/share/bus.db"},
		{"linux", "/home/u/a dir/bus.db", "file:///home/u/a%20dir/bus.db"},
		{"darwin", `/tmp/back\slash.db`, "file:///tmp/back%5Cslash.db"},
	} {
		u := url.URL{Scheme: "file", Path: SQLiteURIPath(c.goos, c.path)}
		if got := u.String(); got != c.want {
			t.Errorf("%s %q: %s, want %s", c.goos, c.path, got, c.want)
		}
	}
}

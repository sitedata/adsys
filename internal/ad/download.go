package ad

/*
System

On startup (after sssd) or refresh:
Create a ticket from keytab:
$ kinit 'ad-desktop-1$@EXAMPLE.COM' -k -c /run/adsys/krb5cc/<FQDN>
<download call for host>

User

* On login pam_sss sets KRB5CCNAME
Client passes KRB5CCNAME to daemon
Daemon verifies that it matches the uid of the caller
Creates a symlink in /run/adsys/krb5cc/username -> /tmp/krb5cc_…
<download call for user>:

* On refresh:
systemd system unit timer
List all /run/adsys/krb5cc/
Check the symlink is not dangling
Check the user is still logged in (loginctl?)
For each logged in user (sequentially):
- <download call for user>

<download call>
  mutex for download
  set KRB5CCNAME
  download all GPO concurrently
  unset KRB5CCNAME
  release mutex

*/

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mvo5/libsmbclient-go"
	log "github.com/ubuntu/adsys/internal/grpc/logstreamer"
	"github.com/ubuntu/adsys/internal/i18n"
	"github.com/ubuntu/adsys/internal/smbsafe"
	"github.com/ubuntu/decorate"
	"golang.org/x/sync/errgroup"
	"gopkg.in/ini.v1"
)

/*
fetch downloads a list of gpos from a url for a given kerberosTicket and stores the downloaded files in dest.
In addition, assetsURL is always refreshed if not empty.
Each gpo entry must be a gpo, with a name, url of the form: smb://<server>/SYSVOL/<AD domain>/<GPO_ID> and mutex.
If krb5Ticket is empty, no authentication is done on samba.
This should not be called concurrently.

It returns if the assets were refreshed or not.
*/
func (ad *AD) fetch(ctx context.Context, krb5Ticket string, downloadables map[string]string) (assetsWereRefreshed bool, err error) {
	defer decorate.OnError(&err, i18n.G("can't download all gpos and assets"))

	// protect env variable and map creation
	ad.fetchMu.Lock()
	defer ad.fetchMu.Unlock()

	// Set kerberos ticket.
	const krb5TicketEnv = "KRB5CCNAME"
	oldKrb5Ticket := os.Getenv(krb5TicketEnv)
	if err := os.Setenv(krb5TicketEnv, krb5Ticket); err != nil {
		return false, err
	}
	defer func() {
		if err := os.Setenv(krb5TicketEnv, oldKrb5Ticket); err != nil {
			log.Errorf(ctx, "Couln't restore initial value for %s: %v", krb5Ticket, err)
		}
	}()

	client := libsmbclient.New()
	defer client.Close()
	// When testing we cannot use kerberos without a real kerberos server
	// So we don't use kerberos in this case
	if !ad.withoutKerberos {
		client.SetUseKerberos()
	}

	var errg errgroup.Group
	for name, url := range downloadables {
		g, ok := ad.downloadables[name]
		if !ok {
			ad.downloadables[name] = &downloadable{
				name:     name,
				url:      url,
				mu:       &sync.RWMutex{},
				isAssets: false,
			}
			if name == "assets" {
				ad.downloadables[name].isAssets = true
			}
			g = ad.downloadables[name]
		}
		errg.Go(func() (err error) {
			defer decorate.OnError(&err, i18n.G("can't download %q"), g.name)

			smbsafe.WaitSmb()
			defer smbsafe.DoneSmb()

			log.Debugf(ctx, "Analyzing %q", g.name)

			dest := filepath.Join(ad.sysvolCacheDir, "Policies", filepath.Base(g.url))
			if g.isAssets {
				dest = filepath.Join(ad.sysvolCacheDir, "assets")
			}

			// Look at GPO version and compare with the one on AD to decide if we redownload or not
			shouldDownload, err := needsDownload(ctx, client, g, dest)
			if err != nil {
				if g.isAssets && errors.Is(err, errNoGPTINI) {
					log.Info(ctx, "No assets directory with GPT.INI file found on AD, skipping assets download")
					if _, err := os.Stat(dest); err == nil {
						// we remove the assets existing directory. We need to repack the db.
						assetsWereRefreshed = true
						if err := os.RemoveAll(dest); err != nil {
							return err
						}
					}
					return nil
				}
				return err
			}

			if !shouldDownload {
				if g.isAssets {
					log.Infof(ctx, i18n.G("Assets directory is already up to date"))
				} else {
					log.Infof(ctx, i18n.G("GPO %q is already up to date"), g.name)
				}

				return nil
			}

			log.Infof(ctx, "Downloading %q", g.name)
			g.mu.Lock()
			defer g.mu.Unlock()
			g.testConcurrent = true
			if g.isAssets {
				assetsWereRefreshed = true
			}

			return downloadDir(ctx, client, g.url, dest)
		})
	}

	if err := errg.Wait(); err != nil {
		return false, fmt.Errorf("one or more error while fetching GPOs and assets: %w", err)
	}

	return assetsWereRefreshed, nil
}

var errNoGPTINI = errors.New("no GPT.INI file")

// needsDownload returns if the downloadable should be refreshed.
// This is done by comparing GPT.INI Version= content.
func needsDownload(ctx context.Context, client *libsmbclient.Client, g *downloadable, localPath string) (updateNeeded bool, err error) {
	defer decorate.OnError(&err, i18n.G("can't check if %s needs refreshing"), g.name)

	g.mu.RLock()
	defer g.mu.RUnlock()

	var localVersion, remoteVersion int
	if gptIniPath, err := findLocalGPTIni(localPath); err == nil {
		if f, err := os.Open(filepath.Clean(gptIniPath)); err == nil {
			defer decorate.LogFuncOnErrorContext(ctx, f.Close)

			if localVersion, err = getGPOVersion(ctx, f, g.name); err != nil {
				log.Warningf(ctx, "Invalid local GPT.INI for %s: %v\nDownloading it again…", g.name, err)
			}
		}
	}

	f, err := client.Open(fmt.Sprintf("%s/GPT.INI", g.url), 0, 0)
	if err != nil {
		// nolint:errorlint // We cannot have multiple error wrapping directives in a single call
		return false, fmt.Errorf("%w: %v", errNoGPTINI, err)
	}
	defer f.Close()
	// Read() is on *libsmbclient.File, not libsmbclient.File
	pf := &f
	if remoteVersion, err = getGPOVersion(ctx, pf, g.name); err != nil {
		return false, err
	}

	log.Debugf(ctx, "Local version for %q: %d, remote version: %d", g.name, localVersion, remoteVersion)
	if localVersion >= remoteVersion {
		return false, nil
	}

	return true, nil
}

func getGPOVersion(ctx context.Context, r io.Reader, downloadableName string) (version int, err error) {
	defer decorate.OnError(&err, i18n.G("invalid remote GPT.INI"))

	buf, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}

	cfg, err := ini.Load(buf)
	if err != nil {
		return 0, err
	}

	// If the file exists but doesn't contain a Version key, we log a message and return 0
	// This is the case for some Default Domain Policy GPOs
	if !cfg.Section("General").HasKey("Version") {
		log.Infof(ctx, i18n.G("No version key found in GPT.INI for %s, assuming 0"), downloadableName)
		return 0, nil
	}

	version, err = cfg.Section("General").Key("Version").Int()
	if err != nil {
		return 0, err
	}
	return version, nil
}

// downloadDir will dl in a temporary directory and only commit it if fully downloaded without any errors.
func downloadDir(ctx context.Context, client *libsmbclient.Client, url, dest string) (err error) {
	defer decorate.OnError(&err, i18n.G("download %q failed"), url)

	smbsafe.WaitSmb()
	defer smbsafe.DoneSmb()

	// Check if we have a file or a directory
	d, err := client.Opendir(url)
	if err != nil {
		return err
	}

	// It is a directory: recursive download
	if err := d.Closedir(); err != nil {
		return fmt.Errorf(i18n.G("could not close directory: %v"), err)
	}

	tmpdest, err := os.MkdirTemp(filepath.Dir(dest), fmt.Sprintf("%s.*", filepath.Base(dest)))
	if err != nil {
		return err
	}
	// Always to try remove temporary directory, so that in case of any failures, it’s not left behind
	defer func() {
		if err := os.RemoveAll(tmpdest); err != nil {
			log.Info(ctx, i18n.G("Could not clean up temporary directory:"), err)
		}
	}()
	if err := downloadRecursive(ctx, client, url, tmpdest); err != nil {
		return err
	}
	// Remove previous download content
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	// Rename temporary directory to final location
	if err := os.Rename(tmpdest, dest); err != nil {
		return err
	}
	return nil
}

func downloadRecursive(ctx context.Context, client *libsmbclient.Client, url, dest string) error {
	d, err := client.Opendir(url)
	if err != nil {
		return err
	}
	defer func() {
		if err := d.Closedir(); err != nil {
			log.Info(ctx, "Could not close directory:", err)
		}
	}()

	if err := os.MkdirAll(dest, 0700); err != nil {
		return fmt.Errorf("can't create %q", dest)
	}

	for {
		dirent, err := d.Readdir()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		if dirent.Name == "." || dirent.Name == ".." {
			continue
		}

		entityURL := url + "/" + dirent.Name
		entityDest := filepath.Join(dest, dirent.Name)

		switch dirent.Type {
		case libsmbclient.SmbcFile:
			log.Debugf(ctx, i18n.G("Downloading %s"), entityURL)
			f, err := client.Open(entityURL, 0, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			// Read() is on *libsmbclient.File, not libsmbclient.File
			pf := &f
			data, err := io.ReadAll(pf)
			if err != nil {
				return err
			}

			if err := os.WriteFile(entityDest, data, 0600); err != nil {
				return err
			}
		case libsmbclient.SmbcDir:
			err := downloadRecursive(ctx, client, entityURL, entityDest)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported type %q for entry %s", dirent.Type, dirent.Name)
		}
	}
	return nil
}

// findLocalGPTIni will look for a GPT.INI file in the given path (non-recursive).
// To account for case differences in the filename/extension, try the canonical
// name first (all uppercase), then walk the directory and check each entry.
func findLocalGPTIni(path string) (string, error) {
	if _, err := os.Stat(filepath.Join(path, "GPT.INI")); err == nil {
		return filepath.Join(path, "GPT.INI"), nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf(i18n.G("could not read directory %q: %w"), path, err)
	}

	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), "GPT.INI") {
			return filepath.Join(path, entry.Name()), nil
		}
	}

	return "", fmt.Errorf(i18n.G("could not find GPT.INI in %q"), path)
}

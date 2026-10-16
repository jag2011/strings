// Command stringsd runs the web app and offers a few admin commands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jag2011/strings/internal/ai"
	"github.com/jag2011/strings/internal/arb"
	"github.com/jag2011/strings/internal/store"
	"github.com/jag2011/strings/internal/web"
)

const usage = `stringsd: the strings server, minimalistic string management

Usage:
  stringsd serve  [-db strings.db] [-addr :8080] [-base /strings] [-trust-proxy]
  stringsd user   [-db strings.db] [-admin | -proofreader] [-url http://localhost:8080] <email> <name>
  stringsd import [-db strings.db] <file.arb>...
  stringsd export [-db strings.db] [-out .]
  stringsd token  [-db strings.db] <name>
  stringsd backup [-db strings.db] <out.db>

The database path can also be set with STRINGS_DB, the listen address with
STRINGS_ADDR. -base (or STRINGS_BASE) serves the app under a path, like
/strings, instead of the root. Behind a load balancer that appends
X-Forwarded-For, set -trust-proxy (or STRINGS_TRUST_PROXY=1) so login limits
apply per client.

Sign-in links from an external account server (see README) are enabled with
STRINGS_AUTH_URL (the code exchange endpoint), STRINGS_AUTH_SECRET and
STRINGS_AUTH_NAME (shown to users, e.g. "Home Quest").

AI translation (Admin → AI) is enabled with OPENROUTER_API_KEY.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dbPath := fs.String("db", envOr("STRINGS_DB", "strings.db"), "SQLite database file")

	var err error
	switch cmd {
	case "serve":
		addr := fs.String("addr", envOr("STRINGS_ADDR", ":8080"), "listen address")
		base := fs.String("base", os.Getenv("STRINGS_BASE"), "path to serve under, like /strings")
		trustProxy := fs.Bool("trust-proxy", os.Getenv("STRINGS_TRUST_PROXY") == "1", "take the client IP from X-Forwarded-For")
		fs.Parse(args)
		*base = strings.TrimSuffix(*base, "/")
		if *base != "" && (!strings.HasPrefix(*base, "/") || strings.Contains(*base, "//")) {
			log.Fatalf("stringsd: -base must be a path like /strings, not %q", *base)
		}
		var ext *web.External
		ext, err = externalFromEnv()
		if err == nil {
			err = serve(*dbPath, *addr, *base, ext, *trustProxy)
		}
	case "user":
		admin := fs.Bool("admin", false, "make the user an admin")
		proofreader := fs.Bool("proofreader", false, "make the user a proofreader, who can approve translations")
		base := fs.String("url", "http://localhost:8080", "public URL, for the invite link")
		fs.Parse(args)
		if fs.NArg() != 2 || (*admin && *proofreader) {
			fs.Usage()
			os.Exit(2)
		}
		role := "translator"
		if *admin {
			role = "admin"
		} else if *proofreader {
			role = "proofreader"
		}
		err = addUser(*dbPath, fs.Arg(0), fs.Arg(1), role, *base)
	case "import":
		fs.Parse(args)
		err = importFiles(*dbPath, fs.Args())
	case "export":
		out := fs.String("out", ".", "output directory")
		fs.Parse(args)
		err = export(*dbPath, *out)
	case "token":
		fs.Parse(args)
		if fs.NArg() != 1 {
			fs.Usage()
			os.Exit(2)
		}
		err = withStore(*dbPath, func(s *store.Store) error {
			t, err := s.CreateAPIToken(context.Background(), fs.Arg(0))
			if err == nil {
				fmt.Println(t)
			}
			return err
		})
	case "backup":
		fs.Parse(args)
		if fs.NArg() != 1 {
			fs.Usage()
			os.Exit(2)
		}
		// Opening a missing database would create an empty one to back up.
		if _, err = os.Stat(*dbPath); err == nil {
			err = withStore(*dbPath, func(s *store.Store) error {
				return s.Backup(context.Background(), fs.Arg(0))
			})
		}
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal("stringsd: ", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func withStore(path string, fn func(*store.Store) error) error {
	s, err := store.Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
}

func externalFromEnv() (*web.External, error) {
	x := &web.External{URL: os.Getenv("STRINGS_AUTH_URL"), Secret: os.Getenv("STRINGS_AUTH_SECRET"), Name: os.Getenv("STRINGS_AUTH_NAME")}
	switch {
	case x.URL == "" && x.Secret == "":
		return nil, nil
	case x.URL == "" || x.Secret == "" || x.Name == "":
		return nil, fmt.Errorf("STRINGS_AUTH_URL, STRINGS_AUTH_SECRET and STRINGS_AUTH_NAME must be set together")
	}
	return x, nil
}

func serve(dbPath, addr, base string, ext *web.External, trustProxy bool) error {
	return withStore(dbPath, func(s *store.Store) error {
		srv, err := web.New(s, ext)
		if err != nil {
			return err
		}
		srv.TrustProxy = trustProxy
		srv.Base = base
		srv.Models = ai.NewCatalog()
		hs := &http.Server{
			Addr:              addr,
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			// Longer than the load balancer's 60s idle timeout, so the
			// server never closes a connection the balancer is about to reuse.
			IdleTimeout: 2 * time.Minute,
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
			srv.AI = ai.NewRunner(s, ai.NewOpenRouter(key))
			srv.AI.LangName = web.LangName
			if err := srv.AI.Start(ctx); err != nil {
				return err
			}
			// A running job is stopped with the server and marked
			// interrupted before the database closes.
			defer func() {
				stop()
				srv.AI.Wait()
			}()
			log.Printf("stringsd: AI translation through OpenRouter")
		} else if _, err := s.InterruptAIRuns(ctx); err != nil {
			return err
		}
		go func() {
			<-ctx.Done()
			shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			hs.Shutdown(shut)
		}()
		log.Printf("stringsd: serving %s on %s%s", dbPath, addr, base)
		if ext != nil {
			log.Printf("stringsd: sign-in links from %s (%s)", ext.Name, ext.URL)
		}
		if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
}

func addUser(dbPath, email, name, role, base string) error {
	return withStore(dbPath, func(s *store.Store) error {
		ctx := context.Background()
		u, err := s.CreateUser(ctx, email, name, role)
		if err != nil {
			return err
		}
		token, err := s.CreateUserToken(ctx, u.ID)
		if err != nil {
			return err
		}
		fmt.Printf("Created %s (%s). Set the password here within 7 days:\n%s/invite/%s\n",
			u.Email, u.Role, strings.TrimSuffix(base, "/"), token)
		return nil
	})
}

func importFiles(dbPath string, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no files given")
	}
	files := map[string][]byte{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.Base(p)] = data
	}
	return withStore(dbPath, func(s *store.Store) error {
		res, err := s.ImportFiles(context.Background(), files, nil)
		if err != nil {
			return err
		}
		failed := false
		for _, r := range res {
			switch {
			case r.Err != nil:
				failed = true
				fmt.Printf("%-20s error: %v\n", r.File, r.Err)
			case r.Source != nil:
				fmt.Printf("%-20s source: revision %d, %d added, %d changed, %d removed, %d unchanged\n",
					r.File, r.Source.Revision, r.Source.Added, r.Source.Changed, r.Source.Removed, r.Source.Unchanged)
			default:
				t := r.Trans
				fmt.Printf("%-20s %d added, %d updated, %d unchanged\n", r.File, t.Added, t.Updated, t.Unchanged)
				if n := len(t.Untranslated); n > 0 {
					fmt.Printf("%-20s   %d same as the source text, left missing\n", "", n)
				}
				for _, l := range []struct {
					what string
					keys []string
				}{
					{"kept, edited here", t.Skipped},
					{"not in source", t.Unknown},
					{"invalid, not imported", t.Invalid},
					{"empty, left missing", t.Empty},
					{"imported with warnings", t.Warned},
				} {
					if len(l.keys) > 0 {
						fmt.Printf("%-20s   %s: %s\n", "", l.what, strings.Join(l.keys, ", "))
					}
				}
			}
		}
		if failed {
			return fmt.Errorf("some files failed")
		}
		return nil
	})
}

func export(dbPath, out string) error {
	return withStore(dbPath, func(s *store.Store) error {
		ctx := context.Background()
		set, err := s.Settings(ctx)
		if err != nil {
			return err
		}
		langs, err := s.Languages(ctx)
		if err != nil {
			return err
		}
		// The source file belongs to the app repo and is not exported.
		for _, lang := range langs {
			f, err := s.Export(ctx, lang)
			if err != nil {
				return err
			}
			path := filepath.Join(out, set["file_prefix"]+lang+".arb")
			if err := os.WriteFile(path, arb.Write(f), 0o644); err != nil {
				return err
			}
			fmt.Println(path)
		}
		return nil
	})
}

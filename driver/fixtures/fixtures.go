package fixtures

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gopkg.in/bblfsh/sdk.v2/driver"
	"gopkg.in/bblfsh/sdk.v2/protocol/v1"
	"gopkg.in/bblfsh/sdk.v2/uast"
	"gopkg.in/bblfsh/sdk.v2/uast/nodes"
	"gopkg.in/bblfsh/sdk.v2/uast/transformer/positioner"
	"gopkg.in/bblfsh/sdk.v2/uast/viewer"
	"gopkg.in/bblfsh/sdk.v2/uast/yaml"
)

const Dir = "fixtures"

const (
	syntaxErrTestName = "_syntax_error"
	maxParseErrors    = 3
	parseTimeout      = time.Minute
)

type SemanticConfig struct {
	// BlacklistTypes is a list og types that should not appear in semantic UAST.
	// Used to test if all cases of a specific native AST type were converted to semantic UAST.
	BlacklistTypes []string
}

// DockerConfig was used to run fixture tests locally using "go test".
//
// Deprecated: use "bblfsh-sdk test"
type DockerConfig struct {
	Debug bool
	Image string
}

type Suite struct {
	Lang string
	Ext  string // with dot
	Path string

	// Update* and Write* flags below should never be committed in "true" state.
	// They serve only as helpers for debugging.

	UpdateNative      bool // update native ASTs in fixtures to ones produced by driver
	UpdateUAST        bool // update UASTs in fixtures to ones produced by driver
	WriteViewerJSON   bool // write JSON compatible with uast-viewer
	WritePreprocessed bool // write a preprocessed UAST for fixtures

	NewDriver  func() driver.Native
	Transforms driver.Transforms

	BenchName string // fixture name to benchmark (with no extension)

	Semantic SemanticConfig
	Docker   DockerConfig

	// VerifyTokens checks that token and positional info matches.
	// Executed after the preprocessing stage (in annotated mode).
	VerifyTokens []positioner.VerifyToken
}

func (s *Suite) fixturesPath(name string) string {
	return filepath.Join(s.Path, name)
}
func (s *Suite) readFixturesFile(t testing.TB, name string) string {
	data, err := ioutil.ReadFile(s.fixturesPath(name))
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

func (s *Suite) writeFixturesFile(t testing.TB, name string, data string) {
	err := ioutil.WriteFile(s.fixturesPath(name), []byte(data), 0666)
	require.NoError(t, err)
}

func (s *Suite) writeViewerJSON(t testing.TB, name string, code string, ast nodes.Node) {
	data, err := viewer.MarshalUAST(s.Lang, code, ast)
	require.NoError(t, err)
	s.writeFixturesFile(t, name+".json", string(data))
}

func (s *Suite) deleteFixturesFile(name string) {
	os.Remove(filepath.Join(s.Path, name))
}

func (s *Suite) RunTests(t *testing.T) {
	if s.Docker.Image != "" && runInDocker {
		s.runTestsDocker(t)
		return
	}
	t.Run("native", s.testFixturesNative)
	t.Run("uast", func(t *testing.T) {
		s.testFixturesUAST(t, driver.ModeAnnotated, uastExt)
	})
	t.Run("semantic", func(t *testing.T) {
		s.testFixturesUAST(t, driver.ModeSemantic, highExt, s.Semantic.BlacklistTypes...)
	})
}

func (s *Suite) RunBenchmarks(b *testing.B) {
	b.Run("transform", func(b *testing.B) {
		s.benchmarkTransform(b, false)
	})
	b.Run("transform-legacy", func(b *testing.B) {
		s.benchmarkTransform(b, true)
	})
	b.Run("fixtures", s.benchmarkFixtures)
}

const (
	gotSuffix = "_got"
	nativeExt = ".native"
	preExt    = ".pre.uast"
	uastExt   = ".uast"
	highExt   = ".sem.uast"
)

func marshalNative(o nodes.Node) ([]byte, error) {
	return uastyml.Marshal(o)
}

func marshalUAST(o nodes.Node) ([]byte, error) {
	return uastyml.Marshal(o)
}

func isTest(name, ext string) (string, bool) {
	if !strings.HasSuffix(name, ext) {
		return "", false
	}
	return strings.TrimSuffix(name, ext), true
}

func (s *Suite) testFixturesNative(t *testing.T) {
	list, err := ioutil.ReadDir(s.Path)
	require.NoError(t, err)

	dr := s.NewDriver()

	err = dr.Start()
	require.NoError(t, err)
	defer dr.Close()

	var parseErrors uint32

	suffix := s.Ext
	for _, ent := range list {
		fname := ent.Name()
		name, ok := isTest(fname, suffix)
		if !ok {
			continue
		} else if atomic.LoadUint32(&parseErrors) >= maxParseErrors {
			return
		}

		t.Run(name, func(t *testing.T) {
			if atomic.LoadUint32(&parseErrors) >= maxParseErrors {
				t.SkipNow()
			}
			code := s.readFixturesFile(t, fname)

			ctx, cancel := context.WithTimeout(context.Background(), parseTimeout)
			resp, err := dr.Parse(ctx, string(code))
			cancel()
			if strings.Contains(fname, syntaxErrTestName) {
				require.True(t, err != nil && !driver.ErrDriverFailure.Is(err), "unexpected error: %v", err)
				return
			}
			if err != nil {
				atomic.AddUint32(&parseErrors, 1)
			}
			require.NoError(t, err)

			js, err := marshalNative(resp)
			require.NoError(t, err)

			exp := s.readFixturesFile(t, fname+nativeExt)
			got := string(js)
			if exp == "" {
				s.writeFixturesFile(t, fname+nativeExt, got)
				t.Skip("no test file found - generating")
			}
			if !assert.ObjectsAreEqual(exp, got) {
				ext := nativeExt + gotSuffix
				if s.UpdateNative {
					ext = nativeExt
				}
				s.writeFixturesFile(t, fname+ext, got)
				if !s.UpdateNative {
					require.Fail(t, "unexpected AST returned by the driver",
						"run diff command to debug:\ndiff -d ./%s ./%s",
						strings.TrimLeft(s.fixturesPath(fname+ext), "./"),
						strings.TrimLeft(s.fixturesPath(fname+nativeExt), "./"),
					)
				} else {
					t.Skip("force update of native fixtures")
				}
			} else {
				s.deleteFixturesFile(fname + nativeExt + gotSuffix)
			}
		})
	}
}

func (s *Suite) testFixturesUAST(t *testing.T, mode driver.Mode, suf string, blacklist ...string) {
	ctx := context.Background()

	list, err := ioutil.ReadDir(s.Path)
	require.NoError(t, err)

	dr := s.NewDriver()

	err = dr.Start()
	require.NoError(t, err)
	defer dr.Close()

	var parseErrors uint32

	suffix := s.Ext
	for _, ent := range list {
		fname := ent.Name()
		name, ok := isTest(fname, suffix)
		if !ok {
			continue
		} else if atomic.LoadUint32(&parseErrors) >= maxParseErrors {
			return
		}

		t.Run(name, func(t *testing.T) {
			if atomic.LoadUint32(&parseErrors) >= maxParseErrors {
				t.SkipNow()
			}
			name += suffix
			code := s.readFixturesFile(t, fname)

			ctx, cancel := context.WithTimeout(ctx, parseTimeout)
			ast, err := dr.Parse(ctx, string(code))
			cancel()
			if err != nil {
				atomic.AddUint32(&parseErrors, 1)
			}
			if strings.Contains(fname, syntaxErrTestName) {
				require.True(t, err != nil && !driver.ErrDriverFailure.Is(err), "unexpected error: %v", err)
				return
			}
			require.NoError(t, err)

			tr := s.Transforms
			if s.WritePreprocessed {
				ua, err := tr.Do(ctx, driver.ModePreprocessed, code, ast)
				require.NoError(t, err)

				un, err := marshalUAST(ua)
				require.NoError(t, err)

				s.writeFixturesFile(t, fname+preExt, string(un))
			}
			ua, err := tr.Do(ctx, mode, code, ast)
			require.NoError(t, err)

			if len(blacklist) != 0 {
				foundBlack := make(map[string]int, len(blacklist))
				for _, typ := range blacklist {
					foundBlack[typ] = 0
				}
				nodes.WalkPreOrder(ua, func(n nodes.Node) bool {
					typ := uast.TypeOf(n)
					if typ == "" {
						return true
					}
					if tr.Namespace != "" && strings.HasPrefix(typ, tr.Namespace+":") {
						typ = strings.TrimPrefix(typ, tr.Namespace+":")
					}
					if cnt, ok := foundBlack[typ]; ok {
						foundBlack[typ] = cnt + 1
					}
					return true
				})
				for typ, cnt := range foundBlack {
					if cnt == 0 {
						delete(foundBlack, typ)
						continue
					}
					t.Errorf("blacklisted nodes of type %q (%d) found in the tree", typ, cnt)
				}
			}
			if mode >= driver.ModeSemantic {
				nodes.WalkPreOrder(ua, func(n nodes.Node) bool {
					typ := uast.TypeOf(n)
					if typ == "" {
						return true
					}
					rv, err := uast.NewValue(typ)
					if uast.ErrTypeNotRegistered.Is(err) {
						return true // skip unregistered native types
					} else if err != nil {
						t.Error(err)
						return true
					}
					if err := uast.NodeAs(n, rv); err != nil {
						t.Errorf("type check failed for %q: %v", typ, err)
					}
					return true
				})
			}
			if len(s.VerifyTokens) != 0 && mode == driver.ModeAnnotated {
				for _, v := range s.VerifyTokens {
					if err := v.Verify(code, ua); err != nil {
						t.Error(err)
					}
				}
			}

			un, err := marshalUAST(ua)
			require.NoError(t, err)

			exp := s.readFixturesFile(t, fname+suf)
			got := string(un)
			if exp == "" {
				s.writeFixturesFile(t, fname+suf, got)
				t.Skip("no test file found - generating")
			}
			if !assert.ObjectsAreEqual(exp, got) {
				ext := suf + gotSuffix
				if s.UpdateUAST {
					ext = suf
				}
				s.writeFixturesFile(t, fname+ext, got)
				if !s.UpdateUAST {
					require.Fail(t, "unexpected UAST returned by the driver",
						"run diff command to debug:\ndiff -d ./%s ./%s",
						strings.TrimLeft(s.fixturesPath(fname+ext), "./"),
						strings.TrimLeft(s.fixturesPath(fname+suf), "./"),
					)
				} else {
					t.Skip("force update of fixtures")
				}
			} else {
				s.deleteFixturesFile(fname + suf + gotSuffix)
			}
			if s.WriteViewerJSON {
				s.writeViewerJSON(t, fname+suf, code, ua)
			}
		})
	}
}

func (s *Suite) benchmarkTransform(b *testing.B, legacy bool) {
	if s.BenchName == "" {
		b.SkipNow()
	}
	code := s.readFixturesFile(b, s.BenchName+s.Ext)

	tr := s.Transforms

	dr := s.NewDriver()

	err := dr.Start()
	require.NoError(b, err)
	defer dr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), parseTimeout)
	rast, err := dr.Parse(ctx, string(code))
	cancel()
	if err != nil {
		b.Fatal(err)
	}
	dr.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ast := rast.Clone()

		ua, err := tr.Do(ctx, driver.ModeSemantic, code, ast)
		if err != nil {
			b.Fatal(err)
		}

		if legacy {
			un, err := uast1.ToNode(ua)
			if err != nil {
				b.Fatal(err)
			}
			_ = un
		}
	}
}

func isBench(name, ext string) (string, bool) {
	const prefix = "bench_"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext), true
}

func (s *Suite) benchmarkFixtures(b *testing.B) {
	b.StopTimer()
	ctx := context.Background()

	list, err := ioutil.ReadDir(s.Path)
	require.NoError(b, err)

	dr := s.NewDriver()
	tr := s.Transforms

	err = dr.Start()
	require.NoError(b, err)
	defer dr.Close()

	suffix := s.Ext
	for _, ent := range list {
		fname := ent.Name()
		name, ok := isBench(fname, suffix)
		if !ok {
			continue
		}

		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			code := string(s.readFixturesFile(b, fname))

			b.ReportAllocs()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				ctx, cancel := context.WithTimeout(ctx, parseTimeout)
				ast, err := dr.Parse(ctx, code)
				cancel()
				require.NoError(b, err)

				_, err = tr.Do(ctx, driver.ModeSemantic, code, ast)
				require.NoError(b, err)
			}
		})
	}
}

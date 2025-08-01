package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func mustParseCIDR(v string) net.IPNet {
	_, ne, err := net.ParseCIDR(v)
	if err != nil {
		panic(err)
	}
	if ipv4 := ne.IP.To4(); ipv4 != nil {
		return net.IPNet{IP: ipv4, Mask: ne.Mask[len(ne.Mask)-4 : len(ne.Mask)]}
	}
	return *ne
}

func TestAuthInternal(t *testing.T) {
	for _, outcome := range []string{
		"ok",
		"wrong user",
		"wrong pass",
		"wrong ip",
		"wrong action",
		"wrong path",
	} {
		for _, encryption := range []string{
			"plain",
			"sha256",
			"argon2",
		} {
			t.Run(outcome+" "+encryption, func(t *testing.T) {
				m := Manager{
					Method: conf.AuthMethodInternal,
					InternalUsers: []conf.AuthInternalUser{
						{
							IPs: conf.IPNetworks{mustParseCIDR("127.1.1.1/32")},
							Permissions: []conf.AuthInternalUserPermission{{
								Action: conf.AuthActionPublish,
								Path:   "mypath",
							}},
						},
					},
				}

				switch encryption {
				case "plain":
					m.InternalUsers[0].User = conf.Credential("testuser")
					m.InternalUsers[0].Pass = conf.Credential("testpass")

				case "sha256":
					m.InternalUsers[0].User = conf.Credential("sha256:rl3rgi4NcZkpAEcacZnQ2VuOfJ0FxAqCRaKB/SwdZoQ=")
					m.InternalUsers[0].Pass = conf.Credential("sha256:E9JJ8stBJ7QM+nV4ZoUCeHk/gU3tPFh/5YieiJp6n2w=")

				case "argon2":
					m.InternalUsers[0].User = conf.Credential(
						"argon2:$argon2id$v=19$m=4096,t=3,p=1$MTIzNDU2Nzg$Ux/LWeTgJQPyfMMJo1myR64+o8rALHoPmlE1i/TR+58")
					m.InternalUsers[0].Pass = conf.Credential(
						"argon2:$argon2i$v=19$m=4096,t=3,p=1$MTIzNDU2Nzg$/mrZ42TiTv1mcPnpMUera5oi0SFYbbyueAbdx5sUvWo")
				}

				switch outcome {
				case "ok":
					err := m.Authenticate(&Request{
						Action: conf.AuthActionPublish,
						Path:   "mypath",
						Credentials: &Credentials{
							User: "testuser",
							Pass: "testpass",
						},
						IP: net.ParseIP("127.1.1.1"),
					})
					require.NoError(t, err)

				case "wrong user":
					err := m.Authenticate(&Request{
						Action: conf.AuthActionPublish,
						Path:   "mypath",
						Credentials: &Credentials{
							User: "wrong",
							Pass: "testpass",
						},
						IP: net.ParseIP("127.1.1.1"),
					})
					require.Error(t, err)

				case "wrong pass":
					err := m.Authenticate(&Request{
						Action: conf.AuthActionPublish,
						Path:   "mypath",
						Credentials: &Credentials{
							User: "testuser",
							Pass: "wrong",
						},
						IP: net.ParseIP("127.1.1.1"),
					})
					require.Error(t, err)

				case "wrong ip":
					err := m.Authenticate(&Request{
						Action: conf.AuthActionPublish,
						Path:   "mypath",
						Credentials: &Credentials{
							User: "testuser",
							Pass: "testpass",
						},
						IP: net.ParseIP("127.1.1.2"),
					})
					require.Error(t, err)

				case "wrong action":
					err := m.Authenticate(&Request{
						Action: conf.AuthActionRead,
						Path:   "mypath",
						Credentials: &Credentials{
							User: "testuser",
							Pass: "testpass",
						},
						IP: net.ParseIP("127.1.1.1"),
					})
					require.Error(t, err)

				case "wrong path":
					err := m.Authenticate(&Request{
						Action: conf.AuthActionPublish,
						Path:   "wrong",
						Credentials: &Credentials{
							User: "testuser",
							Pass: "testpass",
						},
						IP: net.ParseIP("127.1.1.1"),
					})
					require.Error(t, err)
				}
			})
		}
	}
}

func TestAuthInternalCustomVerifyFunc(t *testing.T) {
	for _, ca := range []string{"ok", "invalid"} {
		t.Run(ca, func(t *testing.T) {
			m := Manager{
				Method: conf.AuthMethodInternal,
				InternalUsers: []conf.AuthInternalUser{
					{
						User: "myuser",
						Pass: "mypass",
						IPs:  conf.IPNetworks{mustParseCIDR("127.1.1.1/32")},
						Permissions: []conf.AuthInternalUserPermission{{
							Action: conf.AuthActionPublish,
							Path:   "mypath",
						}},
					},
				},
			}

			req1 := &Request{
				Action:      conf.AuthActionPublish,
				Path:        "mypath",
				Credentials: &Credentials{},
				IP:          net.ParseIP("127.1.1.1"),
				CustomVerifyFunc: func(expectedUser, expectedPass string) bool {
					require.Equal(t, "myuser", expectedUser)
					require.Equal(t, "mypass", expectedPass)
					return (ca == "ok")
				},
			}
			err := m.Authenticate(req1)

			if ca == "ok" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestAuthHTTP(t *testing.T) {
	for _, outcome := range []string{"ok", "fail"} {
		t.Run(outcome, func(t *testing.T) {
			firstReceived := false

			httpServ := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodPost, r.Method)
					require.Equal(t, "/auth", r.URL.Path)

					var in struct {
						IP       string `json:"ip"`
						User     string `json:"user"`
						Password string `json:"password"`
						Path     string `json:"path"`
						Protocol string `json:"protocol"`
						ID       string `json:"id"`
						Action   string `json:"action"`
						Query    string `json:"query"`
					}
					err := json.NewDecoder(r.Body).Decode(&in)
					require.NoError(t, err)

					if in.IP != "127.0.0.1" ||
						in.User != "testpublisher" ||
						in.Password != "testpass" ||
						in.Path != "teststream" ||
						in.Protocol != "rtsp" ||
						(firstReceived && in.ID == "") ||
						in.Action != "publish" ||
						(in.Query != "user=testreader&pass=testpass&param=value" &&
							in.Query != "user=testpublisher&pass=testpass&param=value" &&
							in.Query != "param=value") {
						w.WriteHeader(http.StatusBadRequest)
						return
					}

					firstReceived = true
				}),
			}

			ln, err := net.Listen("tcp", "127.0.0.1:9120")
			require.NoError(t, err)

			go httpServ.Serve(ln)
			defer httpServ.Shutdown(context.Background())

			m := Manager{
				Method:      conf.AuthMethodHTTP,
				HTTPAddress: "http://127.0.0.1:9120/auth",
			}

			if outcome == "ok" {
				err = m.Authenticate(&Request{
					Action:   conf.AuthActionPublish,
					Path:     "teststream",
					Query:    "param=value",
					Protocol: ProtocolRTSP,
					Credentials: &Credentials{
						User: "testpublisher",
						Pass: "testpass",
					},
					IP: net.ParseIP("127.0.0.1"),
				})
				require.NoError(t, err)
			} else {
				err = m.Authenticate(&Request{
					Action:   conf.AuthActionPublish,
					Path:     "teststream",
					Query:    "param=value",
					Protocol: ProtocolRTSP,
					Credentials: &Credentials{
						User: "invalid",
						Pass: "testpass",
					},
					IP: net.ParseIP("127.0.0.1"),
				})
				require.Error(t, err)
			}
		})
	}
}

func TestAuthHTTPExclude(t *testing.T) {
	m := Manager{
		Method:      conf.AuthMethodHTTP,
		HTTPAddress: "http://not-to-be-used:9120/auth",
		HTTPExclude: []conf.AuthInternalUserPermission{{
			Action: conf.AuthActionPublish,
		}},
	}

	err := m.Authenticate(&Request{
		Action:   conf.AuthActionPublish,
		Path:     "teststream",
		Query:    "param=value",
		Protocol: ProtocolRTSP,
		Credentials: &Credentials{
			User: "",
			Pass: "",
		},
		IP: net.ParseIP("127.0.0.1"),
	})
	require.NoError(t, err)
}

func TestAuthJWT(t *testing.T) {
	// reference:
	// https://github.com/MicahParks/jwkset/blob/master/examples/http_server/main.go

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	httpServ := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			jwk, err2 := jwkset.NewJWKFromKey(key, jwkset.JWKOptions{
				Metadata: jwkset.JWKMetadataOptions{
					KID: "test-key-id",
				},
			})
			require.NoError(t, err2)

			jwkSet := jwkset.NewMemoryStorage()
			err2 = jwkSet.KeyWrite(context.Background(), jwk)
			require.NoError(t, err2)

			response, err2 := jwkSet.JSONPublic(r.Context())
			if err2 != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(response)
		}),
	}

	ln, err := net.Listen("tcp", "localhost:4567")
	require.NoError(t, err)

	go httpServ.Serve(ln)
	defer httpServ.Shutdown(context.Background())

	type customClaims struct {
		jwt.RegisteredClaims
		MediaMTXPermissions []conf.AuthInternalUserPermission `json:"my_permission_key"`
	}

	claims := customClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "test",
			Subject:   "somebody",
			ID:        "1",
		},
		MediaMTXPermissions: []conf.AuthInternalUserPermission{{
			Action: conf.AuthActionPublish,
			Path:   "mypath",
		}},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header[jwkset.HeaderKID] = "test-key-id"
	ss, err := token.SignedString(key)
	require.NoError(t, err)

	m := Manager{
		Method:      conf.AuthMethodJWT,
		JWTJWKS:     "http://localhost:4567/jwks",
		JWTClaimKey: "my_permission_key",
	}

	req := &Request{
		Action:   conf.AuthActionPublish,
		Path:     "mypath",
		Protocol: ProtocolWebRTC,
		Credentials: &Credentials{
			Token: ss,
		},
		IP: net.ParseIP("127.0.0.1"),
	}
	err = m.Authenticate(req)
	require.NoError(t, err)
}

func TestAuthJWTAsString(t *testing.T) {
	// reference:
	// https://github.com/MicahParks/jwkset/blob/master/examples/http_server/main.go

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)

	httpServ := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			jwk, err2 := jwkset.NewJWKFromKey(key, jwkset.JWKOptions{
				Metadata: jwkset.JWKMetadataOptions{
					KID: "test-key-id",
				},
			})
			require.NoError(t, err2)

			jwkSet := jwkset.NewMemoryStorage()
			err2 = jwkSet.KeyWrite(context.Background(), jwk)
			require.NoError(t, err2)

			response, err2 := jwkSet.JSONPublic(r.Context())
			if err2 != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(response)
		}),
	}

	ln, err := net.Listen("tcp", "localhost:4567")
	require.NoError(t, err)

	go httpServ.Serve(ln)
	defer httpServ.Shutdown(context.Background())

	type customClaims struct {
		jwt.RegisteredClaims
		MediaMTXPermissions string `json:"my_permission_key"`
	}

	enc, err := json.Marshal([]conf.AuthInternalUserPermission{{
		Action: conf.AuthActionPublish,
		Path:   "mypath",
	}})
	require.NoError(t, err)

	claims := customClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "test",
			Subject:   "somebody",
			ID:        "1",
		},
		MediaMTXPermissions: string(enc),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header[jwkset.HeaderKID] = "test-key-id"
	ss, err := token.SignedString(key)
	require.NoError(t, err)

	m := Manager{
		Method:      conf.AuthMethodJWT,
		JWTJWKS:     "http://localhost:4567/jwks",
		JWTClaimKey: "my_permission_key",
	}

	err = m.Authenticate(&Request{
		Action:   conf.AuthActionPublish,
		Path:     "mypath",
		Query:    "param=value",
		Protocol: ProtocolRTSP,
		Credentials: &Credentials{
			Token: ss,
		},
		IP: net.ParseIP("127.0.0.1"),
	})
	require.NoError(t, err)
}

func TestAuthJWTExclude(t *testing.T) {
	m := Manager{
		Method:      conf.AuthMethodJWT,
		JWTJWKS:     "http://localhost:4567/jwks",
		JWTClaimKey: "my_permission_key",
		JWTExclude: []conf.AuthInternalUserPermission{{
			Action: conf.AuthActionPublish,
		}},
	}

	err := m.Authenticate(&Request{
		Action:   conf.AuthActionPublish,
		Path:     "teststream",
		Query:    "param=value",
		Protocol: ProtocolRTSP,
		IP:       net.ParseIP("127.0.0.1"),
	})
	require.NoError(t, err)
}

func TestAuthJWTRefresh(t *testing.T) {
	// reference:
	// https://github.com/MicahParks/jwkset/blob/master/examples/http_server/main.go

	var key *rsa.PrivateKey

	httpServ := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			jwk, err := jwkset.NewJWKFromKey(key, jwkset.JWKOptions{
				Metadata: jwkset.JWKMetadataOptions{
					KID: "test-key-id",
				},
			})
			require.NoError(t, err)

			jwkSet := jwkset.NewMemoryStorage()
			err = jwkSet.KeyWrite(context.Background(), jwk)
			require.NoError(t, err)

			response, err2 := jwkSet.JSONPublic(r.Context())
			if err2 != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(response)
		}),
	}

	ln, err := net.Listen("tcp", "localhost:4567")
	require.NoError(t, err)

	go httpServ.Serve(ln)
	defer httpServ.Shutdown(context.Background())

	m := Manager{
		Method:      conf.AuthMethodJWT,
		JWTJWKS:     "http://localhost:4567/jwks",
		JWTClaimKey: "my_permission_key",
	}

	for i := 0; i < 2; i++ {
		key, err = rsa.GenerateKey(rand.Reader, 1024)
		require.NoError(t, err)

		type customClaims struct {
			jwt.RegisteredClaims
			MediaMTXPermissions []conf.AuthInternalUserPermission `json:"my_permission_key"`
		}

		claims := customClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				NotBefore: jwt.NewNumericDate(time.Now()),
				Issuer:    "test",
				Subject:   "somebody",
				ID:        "1",
			},
			MediaMTXPermissions: []conf.AuthInternalUserPermission{{
				Action: conf.AuthActionPublish,
				Path:   "mypath",
			}},
		}

		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header[jwkset.HeaderKID] = "test-key-id"
		var ss string
		ss, err = token.SignedString(key)
		require.NoError(t, err)

		err = m.Authenticate(&Request{
			Action:   conf.AuthActionPublish,
			Path:     "mypath",
			Query:    "param=value",
			Protocol: ProtocolRTSP,
			Credentials: &Credentials{
				Token: ss,
			},
			IP: net.ParseIP("127.0.0.1"),
		})
		require.NoError(t, err)

		m.RefreshJWTJWKS()
	}
}

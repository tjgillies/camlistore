/*
Copyright 2014 The Camlistore Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package httputil

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"runtime"
	"sync"
	"time"

	"camlistore.org/pkg/hashutil"

	"go4.org/legal"
	"go4.org/wkfs"
)

var (
	poolOnce sync.Once
	pool     *x509.CertPool
)

var (
	sysRootsOnce sync.Once
	sysRootsGood bool
)

// GenSelfTLS generates a self-signed certificate and key for hostname.
func GenSelfTLS(hostname string) (certPEM, keyPEM []byte, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return certPEM, keyPEM, fmt.Errorf("failed to generate private key: %s", err)
	}

	now := time.Now()

	if hostname == "" {
		hostname = "localhost"
	}
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Fatalf("failed to generate serial number: %s", err)
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   hostname,
			Organization: []string{hostname},
		},
		NotBefore:    now.Add(-5 * time.Minute).UTC(),
		NotAfter:     now.AddDate(1, 0, 0).UTC(),
		SubjectKeyId: []byte{1, 2, 3, 4},
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return certPEM, keyPEM, fmt.Errorf("failed to create certificate: %s", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return certPEM, keyPEM, fmt.Errorf("error writing self-signed HTTPS cert: %v", err)
	}
	certPEM = []byte(string(buf.Bytes()))

	buf.Reset()
	if err := pem.Encode(&buf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {
		return certPEM, keyPEM, fmt.Errorf("error writing self-signed HTTPS private key: %v", err)
	}
	keyPEM = buf.Bytes()
	return certPEM, keyPEM, nil
}

// CertFingerprint returns the SHA-256 prefix of the x509 certificate encoded in certPEM.
func CertFingerprint(certPEM []byte) (string, error) {
	p, _ := pem.Decode(certPEM)
	if p == nil {
		return "", errors.New("no valid PEM data found")
	}
	cert, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse certificate: %v", err)
	}
	return hashutil.SHA256Prefix(cert.Raw), nil
}

// CertFingerprints returns a map of hash prefixes of the x509 certificate encoded in
// certPEM. The hashes are keyed by name ("SHA-1", and "SHA-256").
func CertFingerprints(certPEM []byte) (map[string]string, error) {
	p, _ := pem.Decode(certPEM)
	if p == nil {
		return nil, errors.New("no valid PEM data found")
	}
	cert, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %v", err)
	}
	return map[string]string{
		"SHA-1":   hashutil.SHA1Prefix(cert.Raw),
		"SHA-256": hashutil.SHA256Prefix(cert.Raw),
	}, nil
}

// GenSelfTLSFiles generates a self-signed certificate and key for hostname,
// and writes them to the given paths. If it succeeds it also returns
// the SHA256 prefix of the new cert.
func GenSelfTLSFiles(hostname, certPath, keyPath string) (fingerprint string, err error) {
	cert, key, err := GenSelfTLS(hostname)
	if err != nil {
		return "", err
	}
	sig, err := CertFingerprint(cert)
	if err != nil {
		return "", fmt.Errorf("could not get SHA-256 fingerprint of certificate: %v", err)
	}
	if err := wkfs.WriteFile(certPath, cert, 0666); err != nil {
		return "", fmt.Errorf("failed to write self-signed TLS cert: %v", err)
	}
	if err := wkfs.WriteFile(keyPath, key, 0600); err != nil {
		return "", fmt.Errorf("failed to write self-signed TLS key: %v", err)
	}
	return sig, nil
}

// InstallCerts adds Mozilla's Certificate Authority root set to
// http.DefaultTransport's configuration if the current operating
// system's root CAs are not available. (for instance, if running inside
// a Docker container without a filesystem)
func InstallCerts() {
	if !SystemCARootsAvailable() {
		if tr, ok := http.DefaultTransport.(*http.Transport); ok {
			tlsConf := tr.TLSClientConfig
			if tlsConf == nil {
				tlsConf = &tls.Config{}
				tr.TLSClientConfig = tlsConf
			}
			if tlsConf.RootCAs == nil {
				tlsConf.RootCAs = RootCAPool()
			}
		}
	}
}

// RootCAPool returns the Mozilla Root Certificate Authority pool,
// as statically compiled into the binary.
func RootCAPool() *x509.CertPool {
	poolOnce.Do(buildPool)
	return pool
}

func buildPool() {
	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(certData))
}

// SystemCARootsAvailable reports whether the operating system's root
// CA files are available.
func SystemCARootsAvailable() bool {
	sysRootsOnce.Do(checkSystemRoots)
	return sysRootsGood
}

func checkSystemRoots() {
	if runtime.GOOS == "windows" {
		// Windows is special somehow, and won't be running as
		// a static Docker binary anywhere (which is what this
		// whole file is about), so just say it's fine and we
		// won't use the static CA set below.
		sysRootsGood = true
		return
	}

	// Copied from https://golang.org/pkg/crypto/x509/#example_Certificate_Verify
	// because, as of Go1.6, we need to verify against a real certificate, to check
	// if the RootCAs are found.
	const dummyCertPEM = `
-----BEGIN CERTIFICATE-----
MIIDujCCAqKgAwIBAgIIE31FZVaPXTUwDQYJKoZIhvcNAQEFBQAwSTELMAkGA1UE
BhMCVVMxEzARBgNVBAoTCkdvb2dsZSBJbmMxJTAjBgNVBAMTHEdvb2dsZSBJbnRl
cm5ldCBBdXRob3JpdHkgRzIwHhcNMTQwMTI5MTMyNzQzWhcNMTQwNTI5MDAwMDAw
WjBpMQswCQYDVQQGEwJVUzETMBEGA1UECAwKQ2FsaWZvcm5pYTEWMBQGA1UEBwwN
TW91bnRhaW4gVmlldzETMBEGA1UECgwKR29vZ2xlIEluYzEYMBYGA1UEAwwPbWFp
bC5nb29nbGUuY29tMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEfRrObuSW5T7q
5CnSEqefEmtH4CCv6+5EckuriNr1CjfVvqzwfAhopXkLrq45EQm8vkmf7W96XJhC
7ZM0dYi1/qOCAU8wggFLMB0GA1UdJQQWMBQGCCsGAQUFBwMBBggrBgEFBQcDAjAa
BgNVHREEEzARgg9tYWlsLmdvb2dsZS5jb20wCwYDVR0PBAQDAgeAMGgGCCsGAQUF
BwEBBFwwWjArBggrBgEFBQcwAoYfaHR0cDovL3BraS5nb29nbGUuY29tL0dJQUcy
LmNydDArBggrBgEFBQcwAYYfaHR0cDovL2NsaWVudHMxLmdvb2dsZS5jb20vb2Nz
cDAdBgNVHQ4EFgQUiJxtimAuTfwb+aUtBn5UYKreKvMwDAYDVR0TAQH/BAIwADAf
BgNVHSMEGDAWgBRK3QYWG7z2aLV29YG2u2IaulqBLzAXBgNVHSAEEDAOMAwGCisG
AQQB1nkCBQEwMAYDVR0fBCkwJzAloCOgIYYfaHR0cDovL3BraS5nb29nbGUuY29t
L0dJQUcyLmNybDANBgkqhkiG9w0BAQUFAAOCAQEAH6RYHxHdcGpMpFE3oxDoFnP+
gtuBCHan2yE2GRbJ2Cw8Lw0MmuKqHlf9RSeYfd3BXeKkj1qO6TVKwCh+0HdZk283
TZZyzmEOyclm3UGFYe82P/iDFt+CeQ3NpmBg+GoaVCuWAARJN/KfglbLyyYygcQq
0SgeDh8dRKUiaW3HQSoYvTvdTuqzwK4CXsr3b5/dAOY8uMuG/IAR3FgwTbZ1dtoW
RvOTa8hYiU6A475WuZKyEHcwnGYe57u2I2KbMgcKjPniocj4QzgYsVAVKW3IwaOh
yE+vPxsiUkvQHdO2fojCkY8jg70jxM+gu59tPDNbw3Uh/2Ij310FgTHsnGQMyA==
-----END CERTIFICATE-----`

	// Verify a dummy cert just to test whether the system roots
	// are available.  This depends on knowing that the x509
	// package returns this type of error first, before checking
	// the certificate's validity.
	// The dummy cert should not be just an empty cert, because as of Go
	// 1.6, one would get the
	// "x509: missing ASN.1 contents; use ParseCertificate" error before
	// hitting the SystemRootsError.
	// Related: https://github.com/camlistore/camlistore/issues/705
	block, _ := pem.Decode([]byte(dummyCertPEM))
	if block == nil {
		panic("failed to decode dummy certificate PEM")
	}
	dummyCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		panic("failed to parse dummy certificate: " + err.Error())
	}
	_, err = dummyCert.Verify(x509.VerifyOptions{})
	_, isSysRootError := err.(x509.SystemRootsError)
	sysRootsGood = !isSysRootError
}

func init() {
	legal.RegisterLicense(`
For Mozilla Root CA set,
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at http://mozilla.org/MPL/2.0/.

`)
}

// Generated from https://github.com/agl/extract-nss-root-certs
var certData = `
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at http://mozilla.org/MPL/2.0/.

# Issuer: CN=GTE CyberTrust Global Root O=GTE Corporation OU=GTE CyberTrust Solutions, Inc.
# Subject: CN=GTE CyberTrust Global Root O=GTE Corporation OU=GTE CyberTrust Solutions, Inc.
# Label: "GTE CyberTrust Global Root"
# Serial: 421
# MD5 Fingerprint: ca:3d:d3:68:f1:03:5c:d0:32:fa:b8:2b:59:e8:5a:db
# SHA1 Fingerprint: 97:81:79:50:d8:1c:96:70:cc:34:d8:09:cf:79:44:31:36:7e:f4:74
# SHA256 Fingerprint: a5:31:25:18:8d:21:10:aa:96:4b:02:c7:b7:c6:da:32:03:17:08:94:e5:fb:71:ff:fb:66:67:d5:e6:81:0a:36
-----BEGIN CERTIFICATE-----
MIICWjCCAcMCAgGlMA0GCSqGSIb3DQEBBAUAMHUxCzAJBgNVBAYTAlVTMRgwFgYD
VQQKEw9HVEUgQ29ycG9yYXRpb24xJzAlBgNVBAsTHkdURSBDeWJlclRydXN0IFNv
bHV0aW9ucywgSW5jLjEjMCEGA1UEAxMaR1RFIEN5YmVyVHJ1c3QgR2xvYmFsIFJv
b3QwHhcNOTgwODEzMDAyOTAwWhcNMTgwODEzMjM1OTAwWjB1MQswCQYDVQQGEwJV
UzEYMBYGA1UEChMPR1RFIENvcnBvcmF0aW9uMScwJQYDVQQLEx5HVEUgQ3liZXJU
cnVzdCBTb2x1dGlvbnMsIEluYy4xIzAhBgNVBAMTGkdURSBDeWJlclRydXN0IEds
b2JhbCBSb290MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQCVD6C28FCc6HrH
iM3dFw4usJTQGz0O9pTAipTHBsiQl8i4ZBp6fmw8U+E3KHNgf7KXUwefU/ltWJTS
r41tiGeA5u2ylc9yMcqlHHK6XALnZELn+aks1joNrI1CqiQBOeacPwGFVw1Yh0X4
04Wqk2kmhXBIgD8SFcd5tB8FLztimQIDAQABMA0GCSqGSIb3DQEBBAUAA4GBAG3r
GwnpXtlR22ciYaQqPEh346B8pt5zohQDhT37qw4wxYMWM4ETCJ57NE7fQMh017l9
3PR2VX2bY1QY6fDq81yx2YtCHrnAlU66+tXifPVoYb+O7AWXX1uw16OFNMQkpw0P
lZPvy5TYnh+dXIVtx6quTx8itc2VrbqnzPmrC3p/
-----END CERTIFICATE-----

# Issuer: CN=Thawte Server CA O=Thawte Consulting cc OU=Certification Services Division
# Subject: CN=Thawte Server CA O=Thawte Consulting cc OU=Certification Services Division
# Label: "Thawte Server CA"
# Serial: 1
# MD5 Fingerprint: c5:70:c4:a2:ed:53:78:0c:c8:10:53:81:64:cb:d0:1d
# SHA1 Fingerprint: 23:e5:94:94:51:95:f2:41:48:03:b4:d5:64:d2:a3:a3:f5:d8:8b:8c
# SHA256 Fingerprint: b4:41:0b:73:e2:e6:ea:ca:47:fb:c4:2f:8f:a4:01:8a:f4:38:1d:c5:4c:fa:a8:44:50:46:1e:ed:09:45:4d:e9
-----BEGIN CERTIFICATE-----
MIIDEzCCAnygAwIBAgIBATANBgkqhkiG9w0BAQQFADCBxDELMAkGA1UEBhMCWkEx
FTATBgNVBAgTDFdlc3Rlcm4gQ2FwZTESMBAGA1UEBxMJQ2FwZSBUb3duMR0wGwYD
VQQKExRUaGF3dGUgQ29uc3VsdGluZyBjYzEoMCYGA1UECxMfQ2VydGlmaWNhdGlv
biBTZXJ2aWNlcyBEaXZpc2lvbjEZMBcGA1UEAxMQVGhhd3RlIFNlcnZlciBDQTEm
MCQGCSqGSIb3DQEJARYXc2VydmVyLWNlcnRzQHRoYXd0ZS5jb20wHhcNOTYwODAx
MDAwMDAwWhcNMjAxMjMxMjM1OTU5WjCBxDELMAkGA1UEBhMCWkExFTATBgNVBAgT
DFdlc3Rlcm4gQ2FwZTESMBAGA1UEBxMJQ2FwZSBUb3duMR0wGwYDVQQKExRUaGF3
dGUgQ29uc3VsdGluZyBjYzEoMCYGA1UECxMfQ2VydGlmaWNhdGlvbiBTZXJ2aWNl
cyBEaXZpc2lvbjEZMBcGA1UEAxMQVGhhd3RlIFNlcnZlciBDQTEmMCQGCSqGSIb3
DQEJARYXc2VydmVyLWNlcnRzQHRoYXd0ZS5jb20wgZ8wDQYJKoZIhvcNAQEBBQAD
gY0AMIGJAoGBANOkUG7I/1Zr5s9dtuoMaHVHoqrC2oQl/Kj0R1HahbUgdJSGHg91
yekIYfUGbTBuFRkC6VLAYttNmZ7iagxEOM3+vuNkCXDF/rFrKbYvScg71CcEJRCX
L+eQbcAoQpnXTEPew/UhbVSfXcNY4cDk2VuwuNy0e982OsK1ZiIS1ocNAgMBAAGj
EzARMA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZIhvcNAQEEBQADgYEAB/pMaVz7lcxG
7oWDTSEwjsrZqG9JGubaUeNgcGyEYRGhGshIPllDfU+VPaGLtwtimHp1it2ITk6e
QNuozDJ0uW8NxuOzRAvZim+aKZuZGCg70eNAKJpaPNW15yAbi8qkq43pUdniTCxZ
qdq5snUb9kLy78fyGPmJvKP/iiMucEc=
-----END CERTIFICATE-----

# Issuer: CN=Thawte Premium Server CA O=Thawte Consulting cc OU=Certification Services Division
# Subject: CN=Thawte Premium Server CA O=Thawte Consulting cc OU=Certification Services Division
# Label: "Thawte Premium Server CA"
# Serial: 1
# MD5 Fingerprint: 06:9f:69:79:16:66:90:02:1b:8c:8c:a2:c3:07:6f:3a
# SHA1 Fingerprint: 62:7f:8d:78:27:65:63:99:d2:7d:7f:90:44:c9:fe:b3:f3:3e:fa:9a
# SHA256 Fingerprint: ab:70:36:36:5c:71:54:aa:29:c2:c2:9f:5d:41:91:16:3b:16:2a:22:25:01:13:57:d5:6d:07:ff:a7:bc:1f:72
-----BEGIN CERTIFICATE-----
MIIDJzCCApCgAwIBAgIBATANBgkqhkiG9w0BAQQFADCBzjELMAkGA1UEBhMCWkEx
FTATBgNVBAgTDFdlc3Rlcm4gQ2FwZTESMBAGA1UEBxMJQ2FwZSBUb3duMR0wGwYD
VQQKExRUaGF3dGUgQ29uc3VsdGluZyBjYzEoMCYGA1UECxMfQ2VydGlmaWNhdGlv
biBTZXJ2aWNlcyBEaXZpc2lvbjEhMB8GA1UEAxMYVGhhd3RlIFByZW1pdW0gU2Vy
dmVyIENBMSgwJgYJKoZIhvcNAQkBFhlwcmVtaXVtLXNlcnZlckB0aGF3dGUuY29t
MB4XDTk2MDgwMTAwMDAwMFoXDTIwMTIzMTIzNTk1OVowgc4xCzAJBgNVBAYTAlpB
MRUwEwYDVQQIEwxXZXN0ZXJuIENhcGUxEjAQBgNVBAcTCUNhcGUgVG93bjEdMBsG
A1UEChMUVGhhd3RlIENvbnN1bHRpbmcgY2MxKDAmBgNVBAsTH0NlcnRpZmljYXRp
b24gU2VydmljZXMgRGl2aXNpb24xITAfBgNVBAMTGFRoYXd0ZSBQcmVtaXVtIFNl
cnZlciBDQTEoMCYGCSqGSIb3DQEJARYZcHJlbWl1bS1zZXJ2ZXJAdGhhd3RlLmNv
bTCBnzANBgkqhkiG9w0BAQEFAAOBjQAwgYkCgYEA0jY2aovXwlue2oFBYo847kkE
VdbQ7xwblRZH7xhINTpS9CtqBo87L+pW46+GjZ4X9560ZXUCTe/LCaIhUdib0GfQ
ug2SBhRz1JPLlyoAnFxODLz6FVL88kRu2hFKbgifLy3j+ao6hnO2RlNYyIkFvYMR
uHM/qgeN9EJN50CdHDcCAwEAAaMTMBEwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG
9w0BAQQFAAOBgQAmSCwWwlj66BZ0DKqqX1Q/8tfJeGBeXm43YyJ3Nn6yF8Q0ufUI
hfzJATj/Tb7yFkJD57taRvvBxhEf8UqwKEbJw8RCfbz6q1lu1bdRiBHjpIUZa4JM
pAwSremkrj/xw0llmozFyD4lt5SZu5IycQfwhl7tUCemDaYj+bvLpgcUQg==
-----END CERTIFICATE-----

# Issuer: O=Equifax OU=Equifax Secure Certificate Authority
# Subject: O=Equifax OU=Equifax Secure Certificate Authority
# Label: "Equifax Secure CA"
# Serial: 903804111
# MD5 Fingerprint: 67:cb:9d:c0:13:24:8a:82:9b:b2:17:1e:d1:1b:ec:d4
# SHA1 Fingerprint: d2:32:09:ad:23:d3:14:23:21:74:e4:0d:7f:9d:62:13:97:86:63:3a
# SHA256 Fingerprint: 08:29:7a:40:47:db:a2:36:80:c7:31:db:6e:31:76:53:ca:78:48:e1:be:bd:3a:0b:01:79:a7:07:f9:2c:f1:78
-----BEGIN CERTIFICATE-----
MIIDIDCCAomgAwIBAgIENd70zzANBgkqhkiG9w0BAQUFADBOMQswCQYDVQQGEwJV
UzEQMA4GA1UEChMHRXF1aWZheDEtMCsGA1UECxMkRXF1aWZheCBTZWN1cmUgQ2Vy
dGlmaWNhdGUgQXV0aG9yaXR5MB4XDTk4MDgyMjE2NDE1MVoXDTE4MDgyMjE2NDE1
MVowTjELMAkGA1UEBhMCVVMxEDAOBgNVBAoTB0VxdWlmYXgxLTArBgNVBAsTJEVx
dWlmYXggU2VjdXJlIENlcnRpZmljYXRlIEF1dGhvcml0eTCBnzANBgkqhkiG9w0B
AQEFAAOBjQAwgYkCgYEAwV2xWGcIYu6gmi0fCG2RFGiYCh7+2gRvE4RiIcPRfM6f
BeC4AfBONOziipUEZKzxa1NfBbPLZ4C/QgKO/t0BCezhABRP/PvwDN1Dulsr4R+A
cJkVV5MW8Q+XarfCaCMczE1ZMKxRHjuvK9buY0V7xdlfUNLjUA86iOe/FP3gx7kC
AwEAAaOCAQkwggEFMHAGA1UdHwRpMGcwZaBjoGGkXzBdMQswCQYDVQQGEwJVUzEQ
MA4GA1UEChMHRXF1aWZheDEtMCsGA1UECxMkRXF1aWZheCBTZWN1cmUgQ2VydGlm
aWNhdGUgQXV0aG9yaXR5MQ0wCwYDVQQDEwRDUkwxMBoGA1UdEAQTMBGBDzIwMTgw
ODIyMTY0MTUxWjALBgNVHQ8EBAMCAQYwHwYDVR0jBBgwFoAUSOZo+SvSspXXR9gj
IBBPM5iQn9QwHQYDVR0OBBYEFEjmaPkr0rKV10fYIyAQTzOYkJ/UMAwGA1UdEwQF
MAMBAf8wGgYJKoZIhvZ9B0EABA0wCxsFVjMuMGMDAgbAMA0GCSqGSIb3DQEBBQUA
A4GBAFjOKer89961zgK5F7WF0bnj4JXMJTENAKaSbn+2kmOeUJXRmm/kEd5jhW6Y
7qj/WsjTVbJmcVfewCHrPSqnI0kBBIZCe/zuf6IWUrVnZ9NA2zsmWLIodz2uFHdh
1voqZiegDfqnc1zqcPGUIWVEX/r87yloqaKHee9570+sB3c4
-----END CERTIFICATE-----

# Issuer: O=VeriSign, Inc. OU=Class 3 Public Primary Certification Authority - G2/(c) 1998 VeriSign, Inc. - For authorized use only/VeriSign Trust Network
# Subject: O=VeriSign, Inc. OU=Class 3 Public Primary Certification Authority - G2/(c) 1998 VeriSign, Inc. - For authorized use only/VeriSign Trust Network
# Label: "Verisign Class 3 Public Primary Certification Authority - G2"
# Serial: 167285380242319648451154478808036881606
# MD5 Fingerprint: a2:33:9b:4c:74:78:73:d4:6c:e7:c1:f3:8d:cb:5c:e9
# SHA1 Fingerprint: 85:37:1c:a6:e5:50:14:3d:ce:28:03:47:1b:de:3a:09:e8:f8:77:0f
# SHA256 Fingerprint: 83:ce:3c:12:29:68:8a:59:3d:48:5f:81:97:3c:0f:91:95:43:1e:da:37:cc:5e:36:43:0e:79:c7:a8:88:63:8b
-----BEGIN CERTIFICATE-----
MIIDAjCCAmsCEH3Z/gfPqB63EHln+6eJNMYwDQYJKoZIhvcNAQEFBQAwgcExCzAJ
BgNVBAYTAlVTMRcwFQYDVQQKEw5WZXJpU2lnbiwgSW5jLjE8MDoGA1UECxMzQ2xh
c3MgMyBQdWJsaWMgUHJpbWFyeSBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eSAtIEcy
MTowOAYDVQQLEzEoYykgMTk5OCBWZXJpU2lnbiwgSW5jLiAtIEZvciBhdXRob3Jp
emVkIHVzZSBvbmx5MR8wHQYDVQQLExZWZXJpU2lnbiBUcnVzdCBOZXR3b3JrMB4X
DTk4MDUxODAwMDAwMFoXDTI4MDgwMTIzNTk1OVowgcExCzAJBgNVBAYTAlVTMRcw
FQYDVQQKEw5WZXJpU2lnbiwgSW5jLjE8MDoGA1UECxMzQ2xhc3MgMyBQdWJsaWMg
UHJpbWFyeSBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eSAtIEcyMTowOAYDVQQLEzEo
YykgMTk5OCBWZXJpU2lnbiwgSW5jLiAtIEZvciBhdXRob3JpemVkIHVzZSBvbmx5
MR8wHQYDVQQLExZWZXJpU2lnbiBUcnVzdCBOZXR3b3JrMIGfMA0GCSqGSIb3DQEB
AQUAA4GNADCBiQKBgQDMXtERXVxp0KvTuWpMmR9ZmDCOFoUgRm1HP9SFIIThbbP4
pO0M8RcPO/mn+SXXwc+EY/J8Y8+iR/LGWzOOZEAEaMGAuWQcRXfH2G71lSk8UOg0
13gfqLptQ5GVj0VXXn7F+8qkBOvqlzdUMG+7AUcyM83cV5tkaWH4mx0ciU9cZwID
AQABMA0GCSqGSIb3DQEBBQUAA4GBAFFNzb5cy5gZnBWyATl4Lk0PZ3BwmcYQWpSk
U01UbSuvDV1Ai2TT1+7eVmGSX6bEHRBhNtMsJzzoKQm5EWR0zLVznxxIqbxhAe7i
F6YM40AIOw7n60RzKprxaZLvcRTDOaxxp5EJb+RxBrO6WVcmeQD2+A2iMzAo1KpY
oJ2daZH9
-----END CERTIFICATE-----

# Issuer: CN=GlobalSign Root CA O=GlobalSign nv-sa OU=Root CA
# Subject: CN=GlobalSign Root CA O=GlobalSign nv-sa OU=Root CA
# Label: "GlobalSign Root CA"
# Serial: 4835703278459707669005204
# MD5 Fingerprint: 3e:45:52:15:09:51:92:e1:b7:5d:37:9f:b1:87:29:8a
# SHA1 Fingerprint: b1:bc:96:8b:d4:f4:9d:62:2a:a8:9a:81:f2:15:01:52:a4:1d:82:9c
# SHA256 Fingerprint: eb:d4:10:40:e4:bb:3e:c7:42:c9:e3:81:d3:1e:f2:a4:1a:48:b6:68:5c:96:e7:ce:f3:c1:df:6c:d4:33:1c:99
-----BEGIN CERTIFICATE-----
MIIDdTCCAl2gAwIBAgILBAAAAAABFUtaw5QwDQYJKoZIhvcNAQEFBQAwVzELMAkG
A1UEBhMCQkUxGTAXBgNVBAoTEEdsb2JhbFNpZ24gbnYtc2ExEDAOBgNVBAsTB1Jv
b3QgQ0ExGzAZBgNVBAMTEkdsb2JhbFNpZ24gUm9vdCBDQTAeFw05ODA5MDExMjAw
MDBaFw0yODAxMjgxMjAwMDBaMFcxCzAJBgNVBAYTAkJFMRkwFwYDVQQKExBHbG9i
YWxTaWduIG52LXNhMRAwDgYDVQQLEwdSb290IENBMRswGQYDVQQDExJHbG9iYWxT
aWduIFJvb3QgQ0EwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDaDuaZ
jc6j40+Kfvvxi4Mla+pIH/EqsLmVEQS98GPR4mdmzxzdzxtIK+6NiY6arymAZavp
xy0Sy6scTHAHoT0KMM0VjU/43dSMUBUc71DuxC73/OlS8pF94G3VNTCOXkNz8kHp
1Wrjsok6Vjk4bwY8iGlbKk3Fp1S4bInMm/k8yuX9ifUSPJJ4ltbcdG6TRGHRjcdG
snUOhugZitVtbNV4FpWi6cgKOOvyJBNPc1STE4U6G7weNLWLBYy5d4ux2x8gkasJ
U26Qzns3dLlwR5EiUWMWea6xrkEmCMgZK9FGqkjWZCrXgzT/LCrBbBlDSgeF59N8
9iFo7+ryUp9/k5DPAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8E
BTADAQH/MB0GA1UdDgQWBBRge2YaRQ2XyolQL30EzTSo//z9SzANBgkqhkiG9w0B
AQUFAAOCAQEA1nPnfE920I2/7LqivjTFKDK1fPxsnCwrvQmeU79rXqoRSLblCKOz
yj1hTdNGCbM+w6DjY1Ub8rrvrTnhQ7k4o+YviiY776BQVvnGCv04zcQLcFGUl5gE
38NflNUVyRRBnMRddWQVDf9VMOyGj/8N7yy5Y0b2qvzfvGn9LhJIZJrglfCm7ymP
AbEVtQwdpf5pLGkkeB6zpxxxYu7KyJesF12KwvhHhm4qxFYxldBniYUr+WymXUad
DKqC5JlR3XC321Y9YeRq4VzW9v493kHMB65jUr9TU/Qr6cf9tveCX4XSQRjbgbME
HMUfpIBvFSDJ3gyICh3WZlXi/EjJKSZp4A==
-----END CERTIFICATE-----

# Issuer: CN=GlobalSign O=GlobalSign OU=GlobalSign Root CA - R2
# Subject: CN=GlobalSign O=GlobalSign OU=GlobalSign Root CA - R2
# Label: "GlobalSign Root CA - R2"
# Serial: 4835703278459682885658125
# MD5 Fingerprint: 94:14:77:7e:3e:5e:fd:8f:30:bd:41:b0:cf:e7:d0:30
# SHA1 Fingerprint: 75:e0:ab:b6:13:85:12:27:1c:04:f8:5f:dd:de:38:e4:b7:24:2e:fe
# SHA256 Fingerprint: ca:42:dd:41:74:5f:d0:b8:1e:b9:02:36:2c:f9:d8:bf:71:9d:a1:bd:1b:1e:fc:94:6f:5b:4c:99:f4:2c:1b:9e
-----BEGIN CERTIFICATE-----
MIIDujCCAqKgAwIBAgILBAAAAAABD4Ym5g0wDQYJKoZIhvcNAQEFBQAwTDEgMB4G
A1UECxMXR2xvYmFsU2lnbiBSb290IENBIC0gUjIxEzARBgNVBAoTCkdsb2JhbFNp
Z24xEzARBgNVBAMTCkdsb2JhbFNpZ24wHhcNMDYxMjE1MDgwMDAwWhcNMjExMjE1
MDgwMDAwWjBMMSAwHgYDVQQLExdHbG9iYWxTaWduIFJvb3QgQ0EgLSBSMjETMBEG
A1UEChMKR2xvYmFsU2lnbjETMBEGA1UEAxMKR2xvYmFsU2lnbjCCASIwDQYJKoZI
hvcNAQEBBQADggEPADCCAQoCggEBAKbPJA6+Lm8omUVCxKs+IVSbC9N/hHD6ErPL
v4dfxn+G07IwXNb9rfF73OX4YJYJkhD10FPe+3t+c4isUoh7SqbKSaZeqKeMWhG8
eoLrvozps6yWJQeXSpkqBy+0Hne/ig+1AnwblrjFuTosvNYSuetZfeLQBoZfXklq
tTleiDTsvHgMCJiEbKjNS7SgfQx5TfC4LcshytVsW33hoCmEofnTlEnLJGKRILzd
C9XZzPnqJworc5HGnRusyMvo4KD0L5CLTfuwNhv2GXqF4G3yYROIXJ/gkwpRl4pa
zq+r1feqCapgvdzZX99yqWATXgAByUr6P6TqBwMhAo6CygPCm48CAwEAAaOBnDCB
mTAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQUm+IH
V2ccHsBqBt5ZtJot39wZhi4wNgYDVR0fBC8wLTAroCmgJ4YlaHR0cDovL2NybC5n
bG9iYWxzaWduLm5ldC9yb290LXIyLmNybDAfBgNVHSMEGDAWgBSb4gdXZxwewGoG
3lm0mi3f3BmGLjANBgkqhkiG9w0BAQUFAAOCAQEAmYFThxxol4aR7OBKuEQLq4Gs
J0/WwbgcQ3izDJr86iw8bmEbTUsp9Z8FHSbBuOmDAGJFtqkIk7mpM0sYmsL4h4hO
291xNBrBVNpGP+DTKqttVCL1OmLNIG+6KYnX3ZHu01yiPqFbQfXf5WRDLenVOavS
ot+3i9DAgBkcRcAtjOj4LaR0VknFBbVPFd5uRHg5h6h+u/N5GJG79G+dwfCMNYxd
AfvDbbnvRG15RjF+Cv6pgsH/76tuIMRQyV+dTZsXjAzlAcmgQWpzU/qlULRuJQ/7
TBj0/VLZjmmx6BEP3ojY+x1J96relc8geMJgEtslQIxq/H5COEBkEveegeGTLg==
-----END CERTIFICATE-----

# Issuer: CN=VeriSign Class 3 Public Primary Certification Authority - G3 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 1999 VeriSign, Inc. - For authorized use only
# Subject: CN=VeriSign Class 3 Public Primary Certification Authority - G3 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 1999 VeriSign, Inc. - For authorized use only
# Label: "Verisign Class 3 Public Primary Certification Authority - G3"
# Serial: 206684696279472310254277870180966723415
# MD5 Fingerprint: cd:68:b6:a7:c7:c4:ce:75:e0:1d:4f:57:44:61:92:09
# SHA1 Fingerprint: 13:2d:0d:45:53:4b:69:97:cd:b2:d5:c3:39:e2:55:76:60:9b:5c:c6
# SHA256 Fingerprint: eb:04:cf:5e:b1:f3:9a:fa:76:2f:2b:b1:20:f2:96:cb:a5:20:c1:b9:7d:b1:58:95:65:b8:1c:b9:a1:7b:72:44
-----BEGIN CERTIFICATE-----
MIIEGjCCAwICEQCbfgZJoz5iudXukEhxKe9XMA0GCSqGSIb3DQEBBQUAMIHKMQsw
CQYDVQQGEwJVUzEXMBUGA1UEChMOVmVyaVNpZ24sIEluYy4xHzAdBgNVBAsTFlZl
cmlTaWduIFRydXN0IE5ldHdvcmsxOjA4BgNVBAsTMShjKSAxOTk5IFZlcmlTaWdu
LCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxRTBDBgNVBAMTPFZlcmlT
aWduIENsYXNzIDMgUHVibGljIFByaW1hcnkgQ2VydGlmaWNhdGlvbiBBdXRob3Jp
dHkgLSBHMzAeFw05OTEwMDEwMDAwMDBaFw0zNjA3MTYyMzU5NTlaMIHKMQswCQYD
VQQGEwJVUzEXMBUGA1UEChMOVmVyaVNpZ24sIEluYy4xHzAdBgNVBAsTFlZlcmlT
aWduIFRydXN0IE5ldHdvcmsxOjA4BgNVBAsTMShjKSAxOTk5IFZlcmlTaWduLCBJ
bmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxRTBDBgNVBAMTPFZlcmlTaWdu
IENsYXNzIDMgUHVibGljIFByaW1hcnkgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkg
LSBHMzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAMu6nFL8eB8aHm8b
N3O9+MlrlBIwT/A2R/XQkQr1F8ilYcEWQE37imGQ5XYgwREGfassbqb1EUGO+i2t
KmFZpGcmTNDovFJbcCAEWNF6yaRpvIMXZK0Fi7zQWM6NjPXr8EJJC52XJ2cybuGu
kxUccLwgTS8Y3pKI6GyFVxEa6X7jJhFUokWWVYPKMIno3Nij7SqAP395ZVc+FSBm
CC+Vk7+qRy+oRpfwEuL+wgorUeZ25rdGt+INpsyow0xZVYnm6FNcHOqd8GIWC6fJ
Xwzw3sJ2zq/3avL6QaaiMxTJ5Xpj055iN9WFZZ4O5lMkdBteHRJTW8cs54NJOxWu
imi5V5cCAwEAATANBgkqhkiG9w0BAQUFAAOCAQEAERSWwauSCPc/L8my/uRan2Te
2yFPhpk0djZX3dAVL8WtfxUfN2JzPtTnX84XA9s1+ivbrmAJXx5fj267Cz3qWhMe
DGBvtcC1IyIuBwvLqXTLR7sdwdela8wv0kL9Sd2nic9TutoAWii/gt/4uhMdUIaC
/Y4wjylGsB49Ndo4YhYYSq3mtlFs3q9i6wHQHiT+eo8SGhJouPtmmRQURVyu565p
F4ErWjfJXir0xuKhXFSbplQAz/DxwceYMBo7Nhbbo27q/a2ywtrvAkcTisDxszGt
TxzhT5yvDwyd93gN2PQ1VoDat20Xj50egWTh/sVFuq1ruQp6Tk9LhO5L8X3dEQ==
-----END CERTIFICATE-----

# Issuer: CN=VeriSign Class 4 Public Primary Certification Authority - G3 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 1999 VeriSign, Inc. - For authorized use only
# Subject: CN=VeriSign Class 4 Public Primary Certification Authority - G3 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 1999 VeriSign, Inc. - For authorized use only
# Label: "Verisign Class 4 Public Primary Certification Authority - G3"
# Serial: 314531972711909413743075096039378935511
# MD5 Fingerprint: db:c8:f2:27:2e:b1:ea:6a:29:23:5d:fe:56:3e:33:df
# SHA1 Fingerprint: c8:ec:8c:87:92:69:cb:4b:ab:39:e9:8d:7e:57:67:f3:14:95:73:9d
# SHA256 Fingerprint: e3:89:36:0d:0f:db:ae:b3:d2:50:58:4b:47:30:31:4e:22:2f:39:c1:56:a0:20:14:4e:8d:96:05:61:79:15:06
-----BEGIN CERTIFICATE-----
MIIEGjCCAwICEQDsoKeLbnVqAc/EfMwvlF7XMA0GCSqGSIb3DQEBBQUAMIHKMQsw
CQYDVQQGEwJVUzEXMBUGA1UEChMOVmVyaVNpZ24sIEluYy4xHzAdBgNVBAsTFlZl
cmlTaWduIFRydXN0IE5ldHdvcmsxOjA4BgNVBAsTMShjKSAxOTk5IFZlcmlTaWdu
LCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxRTBDBgNVBAMTPFZlcmlT
aWduIENsYXNzIDQgUHVibGljIFByaW1hcnkgQ2VydGlmaWNhdGlvbiBBdXRob3Jp
dHkgLSBHMzAeFw05OTEwMDEwMDAwMDBaFw0zNjA3MTYyMzU5NTlaMIHKMQswCQYD
VQQGEwJVUzEXMBUGA1UEChMOVmVyaVNpZ24sIEluYy4xHzAdBgNVBAsTFlZlcmlT
aWduIFRydXN0IE5ldHdvcmsxOjA4BgNVBAsTMShjKSAxOTk5IFZlcmlTaWduLCBJ
bmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxRTBDBgNVBAMTPFZlcmlTaWdu
IENsYXNzIDQgUHVibGljIFByaW1hcnkgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkg
LSBHMzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAK3LpRFpxlmr8Y+1
GQ9Wzsy1HyDkniYlS+BzZYlZ3tCD5PUPtbut8XzoIfzk6AzufEUiGXaStBO3IFsJ
+mGuqPKljYXCKtbeZjbSmwL0qJJgfJxptI8kHtCGUvYynEFYHiK9zUVilQhu0Gbd
U6LM8BDcVHOLBKFGMzNcF0C5nk3T875Vg+ixiY5afJqWIpA7iCXy0lOIAgwLePLm
NxdLMEYH5IBtptiWLugs+BGzOA1mppvqySNb247i8xOOGlktqgLw7KSHZtzBP/XY
ufTsgsbSPZUd5cBPhMnZo0QoBmrXRazwa2rvTl/4EYIeOGM0ZlDUPpNz+jDDZq3/
ky2X7wMCAwEAATANBgkqhkiG9w0BAQUFAAOCAQEAj/ola09b5KROJ1WrIhVZPMq1
CtRK26vdoV9TxaBXOcLORyu+OshWv8LZJxA6sQU8wHcxuzrTBXttmhwwjIDLk5Mq
g6sFUYICABFna/OIYUdfA5PVWw3g8dShMjWFsjrbsIKr0csKvE+MW8VLADsfKoKm
fjaF3H48ZwC15DtS4KjrXRX5xm3wrR0OhbepmnMUWluPQSjA1egtTaRezarZ7c7c
2NU8Qh0XwRJdRTjDOPP8hS6DRkiy1yBfkjaP53kPmF6Z6PDQpLv1U70qzlmwr25/
bLvSHgCwIe34QWKCudiyxLtGUPMxxY8BqHTr9Xgn2uf3ZkPznoM+IKrDNWCRzg==
-----END CERTIFICATE-----

# Issuer: CN=Entrust.net Certification Authority (2048) O=Entrust.net OU=www.entrust.net/CPS_2048 incorp. by ref. (limits liab.)/(c) 1999 Entrust.net Limited
# Subject: CN=Entrust.net Certification Authority (2048) O=Entrust.net OU=www.entrust.net/CPS_2048 incorp. by ref. (limits liab.)/(c) 1999 Entrust.net Limited
# Label: "Entrust.net Premium 2048 Secure Server CA"
# Serial: 946069240
# MD5 Fingerprint: ee:29:31:bc:32:7e:9a:e6:e8:b5:f7:51:b4:34:71:90
# SHA1 Fingerprint: 50:30:06:09:1d:97:d4:f5:ae:39:f7:cb:e7:92:7d:7d:65:2d:34:31
# SHA256 Fingerprint: 6d:c4:71:72:e0:1c:bc:b0:bf:62:58:0d:89:5f:e2:b8:ac:9a:d4:f8:73:80:1e:0c:10:b9:c8:37:d2:1e:b1:77
-----BEGIN CERTIFICATE-----
MIIEKjCCAxKgAwIBAgIEOGPe+DANBgkqhkiG9w0BAQUFADCBtDEUMBIGA1UEChML
RW50cnVzdC5uZXQxQDA+BgNVBAsUN3d3dy5lbnRydXN0Lm5ldC9DUFNfMjA0OCBp
bmNvcnAuIGJ5IHJlZi4gKGxpbWl0cyBsaWFiLikxJTAjBgNVBAsTHChjKSAxOTk5
IEVudHJ1c3QubmV0IExpbWl0ZWQxMzAxBgNVBAMTKkVudHJ1c3QubmV0IENlcnRp
ZmljYXRpb24gQXV0aG9yaXR5ICgyMDQ4KTAeFw05OTEyMjQxNzUwNTFaFw0yOTA3
MjQxNDE1MTJaMIG0MRQwEgYDVQQKEwtFbnRydXN0Lm5ldDFAMD4GA1UECxQ3d3d3
LmVudHJ1c3QubmV0L0NQU18yMDQ4IGluY29ycC4gYnkgcmVmLiAobGltaXRzIGxp
YWIuKTElMCMGA1UECxMcKGMpIDE5OTkgRW50cnVzdC5uZXQgTGltaXRlZDEzMDEG
A1UEAxMqRW50cnVzdC5uZXQgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkgKDIwNDgp
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEArU1LqRKGsuqjIAcVFmQq
K0vRvwtKTY7tgHalZ7d4QMBzQshowNtTK91euHaYNZOLGp18EzoOH1u3Hs/lJBQe
sYGpjX24zGtLA/ECDNyrpUAkAH90lKGdCCmziAv1h3edVc3kw37XamSrhRSGlVuX
MlBvPci6Zgzj/L24ScF2iUkZ/cCovYmjZy/Gn7xxGWC4LeksyZB2ZnuU4q941mVT
XTzWnLLPKQP5L6RQstRIzgUyVYr9smRMDuSYB3Xbf9+5CFVghTAp+XtIpGmG4zU/
HoZdenoVve8AjhUiVBcAkCaTvA5JaJG/+EfTnZVCwQ5N328mz8MYIWJmQ3DW1cAH
4QIDAQABo0IwQDAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB/zAdBgNV
HQ4EFgQUVeSB0RGAvtiJuQijMfmhJAkWuXAwDQYJKoZIhvcNAQEFBQADggEBADub
j1abMOdTmXx6eadNl9cZlZD7Bh/KM3xGY4+WZiT6QBshJ8rmcnPyT/4xmf3IDExo
U8aAghOY+rat2l098c5u9hURlIIM7j+VrxGrD9cv3h8Dj1csHsm7mhpElesYT6Yf
zX1XEC+bBAlahLVu2B064dae0Wx5XnkcFMXj0EyTO2U87d89vqbllRrDtRnDvV5b
u/8j72gZyxKTJ1wDLW8w0B62GqzeWvfRqqgnpv55gcR5mTNXuhKwqeBCbJPKVt7+
bYQLCIt+jerXmCHG8+c8eS9enNFMFY3h7CI3zJpDC5fcgJCNs2ebb0gIFVbPv/Er
fF6adulZkMV8gzURZVE=
-----END CERTIFICATE-----

# Issuer: CN=Baltimore CyberTrust Root O=Baltimore OU=CyberTrust
# Subject: CN=Baltimore CyberTrust Root O=Baltimore OU=CyberTrust
# Label: "Baltimore CyberTrust Root"
# Serial: 33554617
# MD5 Fingerprint: ac:b6:94:a5:9c:17:e0:d7:91:52:9b:b1:97:06:a6:e4
# SHA1 Fingerprint: d4:de:20:d0:5e:66:fc:53:fe:1a:50:88:2c:78:db:28:52:ca:e4:74
# SHA256 Fingerprint: 16:af:57:a9:f6:76:b0:ab:12:60:95:aa:5e:ba:de:f2:2a:b3:11:19:d6:44:ac:95:cd:4b:93:db:f3:f2:6a:eb
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIEAgAAuTANBgkqhkiG9w0BAQUFADBaMQswCQYDVQQGEwJJ
RTESMBAGA1UEChMJQmFsdGltb3JlMRMwEQYDVQQLEwpDeWJlclRydXN0MSIwIAYD
VQQDExlCYWx0aW1vcmUgQ3liZXJUcnVzdCBSb290MB4XDTAwMDUxMjE4NDYwMFoX
DTI1MDUxMjIzNTkwMFowWjELMAkGA1UEBhMCSUUxEjAQBgNVBAoTCUJhbHRpbW9y
ZTETMBEGA1UECxMKQ3liZXJUcnVzdDEiMCAGA1UEAxMZQmFsdGltb3JlIEN5YmVy
VHJ1c3QgUm9vdDCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAKMEuyKr
mD1X6CZymrV51Cni4eiVgLGw41uOKymaZN+hXe2wCQVt2yguzmKiYv60iNoS6zjr
IZ3AQSsBUnuId9Mcj8e6uYi1agnnc+gRQKfRzMpijS3ljwumUNKoUMMo6vWrJYeK
mpYcqWe4PwzV9/lSEy/CG9VwcPCPwBLKBsua4dnKM3p31vjsufFoREJIE9LAwqSu
XmD+tqYF/LTdB1kC1FkYmGP1pWPgkAx9XbIGevOF6uvUA65ehD5f/xXtabz5OTZy
dc93Uk3zyZAsuT3lySNTPx8kmCFcB5kpvcY67Oduhjprl3RjM71oGDHweI12v/ye
jl0qhqdNkNwnGjkCAwEAAaNFMEMwHQYDVR0OBBYEFOWdWTCCR1jMrPoIVDaGezq1
BE3wMBIGA1UdEwEB/wQIMAYBAf8CAQMwDgYDVR0PAQH/BAQDAgEGMA0GCSqGSIb3
DQEBBQUAA4IBAQCFDF2O5G9RaEIFoN27TyclhAO992T9Ldcw46QQF+vaKSm2eT92
9hkTI7gQCvlYpNRhcL0EYWoSihfVCr3FvDB81ukMJY2GQE/szKN+OMY3EU/t3Wgx
jkzSswF07r51XgdIGn9w/xZchMB5hbgF/X++ZRGjD8ACtPhSNzkE1akxehi/oCr0
Epn3o0WC4zxe9Z2etciefC7IpJ5OCBRLbf1wbWsaY71k5h+3zvDyny67G7fyUIhz
ksLi4xaNmjICq44Y3ekQEe5+NauQrz4wlHrQMz2nZQ/1/I6eYs9HRCwBXbsdtTLS
R9I4LtD+gdwyah617jzV/OeBHRnDJELqYzmp
-----END CERTIFICATE-----

# Issuer: CN=Equifax Secure Global eBusiness CA-1 O=Equifax Secure Inc.
# Subject: CN=Equifax Secure Global eBusiness CA-1 O=Equifax Secure Inc.
# Label: "Equifax Secure Global eBusiness CA"
# Serial: 1
# MD5 Fingerprint: 8f:5d:77:06:27:c4:98:3c:5b:93:78:e7:d7:7d:9b:cc
# SHA1 Fingerprint: 7e:78:4a:10:1c:82:65:cc:2d:e1:f1:6d:47:b4:40:ca:d9:0a:19:45
# SHA256 Fingerprint: 5f:0b:62:ea:b5:e3:53:ea:65:21:65:16:58:fb:b6:53:59:f4:43:28:0a:4a:fb:d1:04:d7:7d:10:f9:f0:4c:07
-----BEGIN CERTIFICATE-----
MIICkDCCAfmgAwIBAgIBATANBgkqhkiG9w0BAQQFADBaMQswCQYDVQQGEwJVUzEc
MBoGA1UEChMTRXF1aWZheCBTZWN1cmUgSW5jLjEtMCsGA1UEAxMkRXF1aWZheCBT
ZWN1cmUgR2xvYmFsIGVCdXNpbmVzcyBDQS0xMB4XDTk5MDYyMTA0MDAwMFoXDTIw
MDYyMTA0MDAwMFowWjELMAkGA1UEBhMCVVMxHDAaBgNVBAoTE0VxdWlmYXggU2Vj
dXJlIEluYy4xLTArBgNVBAMTJEVxdWlmYXggU2VjdXJlIEdsb2JhbCBlQnVzaW5l
c3MgQ0EtMTCBnzANBgkqhkiG9w0BAQEFAAOBjQAwgYkCgYEAuucXkAJlsTRVPEnC
UdXfp9E3j9HngXNBUmCbnaEXJnitx7HoJpQytd4zjTov2/KaelpzmKNc6fuKcxtc
58O/gGzNqfTWK8D3+ZmqY6KxRwIP1ORROhI8bIpaVIRw28HFkM9yRcuoWcDNM50/
o5brhTMhHD4ePmBudpxnhcXIw2ECAwEAAaNmMGQwEQYJYIZIAYb4QgEBBAQDAgAH
MA8GA1UdEwEB/wQFMAMBAf8wHwYDVR0jBBgwFoAUvqigdHJQa0S3ySPY+6j/s1dr
aGwwHQYDVR0OBBYEFL6ooHRyUGtEt8kj2Puo/7NXa2hsMA0GCSqGSIb3DQEBBAUA
A4GBADDiAVGqx+pf2rnQZQ8w1j7aDRRJbpGTJxQx78T3LUX47Me/okENI7SS+RkA
Z70Br83gcfxaz2TE4JaY0KNA4gGK7ycH8WUBikQtBmV1UsCGECAhX2xrD2yuCRyv
8qIYNMR1pHMc8Y3c7635s3a0kr/clRAevsvIO1qEYBlWlKlV
-----END CERTIFICATE-----

# Issuer: CN=Equifax Secure eBusiness CA-1 O=Equifax Secure Inc.
# Subject: CN=Equifax Secure eBusiness CA-1 O=Equifax Secure Inc.
# Label: "Equifax Secure eBusiness CA 1"
# Serial: 4
# MD5 Fingerprint: 64:9c:ef:2e:44:fc:c6:8f:52:07:d0:51:73:8f:cb:3d
# SHA1 Fingerprint: da:40:18:8b:91:89:a3:ed:ee:ae:da:97:fe:2f:9d:f5:b7:d1:8a:41
# SHA256 Fingerprint: cf:56:ff:46:a4:a1:86:10:9d:d9:65:84:b5:ee:b5:8a:51:0c:42:75:b0:e5:f9:4f:40:bb:ae:86:5e:19:f6:73
-----BEGIN CERTIFICATE-----
MIICgjCCAeugAwIBAgIBBDANBgkqhkiG9w0BAQQFADBTMQswCQYDVQQGEwJVUzEc
MBoGA1UEChMTRXF1aWZheCBTZWN1cmUgSW5jLjEmMCQGA1UEAxMdRXF1aWZheCBT
ZWN1cmUgZUJ1c2luZXNzIENBLTEwHhcNOTkwNjIxMDQwMDAwWhcNMjAwNjIxMDQw
MDAwWjBTMQswCQYDVQQGEwJVUzEcMBoGA1UEChMTRXF1aWZheCBTZWN1cmUgSW5j
LjEmMCQGA1UEAxMdRXF1aWZheCBTZWN1cmUgZUJ1c2luZXNzIENBLTEwgZ8wDQYJ
KoZIhvcNAQEBBQADgY0AMIGJAoGBAM4vGbwXt3fek6lfWg0XTzQaDJj0ItlZ1MRo
RvC0NcWFAyDGr0WlIVFFQesWWDYyb+JQYmT5/VGcqiTZ9J2DKocKIdMSODRsjQBu
WqDZQu4aIZX5UkxVWsUPOE9G+m34LjXWHXzr4vCwdYDIqROsvojvOm6rXyo4YgKw
Env+j6YDAgMBAAGjZjBkMBEGCWCGSAGG+EIBAQQEAwIABzAPBgNVHRMBAf8EBTAD
AQH/MB8GA1UdIwQYMBaAFEp4MlIR21kWNl7fwRQ2QGpHfEyhMB0GA1UdDgQWBBRK
eDJSEdtZFjZe38EUNkBqR3xMoTANBgkqhkiG9w0BAQQFAAOBgQB1W6ibAxHm6VZM
zfmpTMANmvPMZWnmJXbMWbfWVMMdzZmsGd20hdXgPfxiIKeES1hl8eL5lSE/9dR+
WB5Hh1Q+WKG1tfgq73HnvMP2sUlG4tega+VWeponmHxGYhTnyfxuAxJ5gDgdSIKN
/Bf+KpYrtWKmpj29f5JZzVoqgrI3eQ==
-----END CERTIFICATE-----

# Issuer: CN=AddTrust Class 1 CA Root O=AddTrust AB OU=AddTrust TTP Network
# Subject: CN=AddTrust Class 1 CA Root O=AddTrust AB OU=AddTrust TTP Network
# Label: "AddTrust Low-Value Services Root"
# Serial: 1
# MD5 Fingerprint: 1e:42:95:02:33:92:6b:b9:5f:c0:7f:da:d6:b2:4b:fc
# SHA1 Fingerprint: cc:ab:0e:a0:4c:23:01:d6:69:7b:dd:37:9f:cd:12:eb:24:e3:94:9d
# SHA256 Fingerprint: 8c:72:09:27:9a:c0:4e:27:5e:16:d0:7f:d3:b7:75:e8:01:54:b5:96:80:46:e3:1f:52:dd:25:76:63:24:e9:a7
-----BEGIN CERTIFICATE-----
MIIEGDCCAwCgAwIBAgIBATANBgkqhkiG9w0BAQUFADBlMQswCQYDVQQGEwJTRTEU
MBIGA1UEChMLQWRkVHJ1c3QgQUIxHTAbBgNVBAsTFEFkZFRydXN0IFRUUCBOZXR3
b3JrMSEwHwYDVQQDExhBZGRUcnVzdCBDbGFzcyAxIENBIFJvb3QwHhcNMDAwNTMw
MTAzODMxWhcNMjAwNTMwMTAzODMxWjBlMQswCQYDVQQGEwJTRTEUMBIGA1UEChML
QWRkVHJ1c3QgQUIxHTAbBgNVBAsTFEFkZFRydXN0IFRUUCBOZXR3b3JrMSEwHwYD
VQQDExhBZGRUcnVzdCBDbGFzcyAxIENBIFJvb3QwggEiMA0GCSqGSIb3DQEBAQUA
A4IBDwAwggEKAoIBAQCWltQhSWDia+hBBwzexODcEyPNwTXH+9ZOEQpnXvUGW2ul
CDtbKRY654eyNAbFvAWlA3yCyykQruGIgb3WntP+LVbBFc7jJp0VLhD7Bo8wBN6n
tGO0/7Gcrjyvd7ZWxbWroulpOj0OM3kyP3CCkplhbY0wCI9xP6ZIVxn4JdxLZlyl
dI+Yrsj5wAYi56xz36Uu+1LcsRVlIPo1Zmne3yzxbrww2ywkEtvrNTVokMsAsJch
PXQhI2U0K7t4WaPW4XY5mqRJjox0r26kmqPZm9I4XJuiGMx1I4S+6+JNM3GOGvDC
+Mcdoq0Dlyz4zyXG9rgkMbFjXZJ/Y/AlyVMuH79NAgMBAAGjgdIwgc8wHQYDVR0O
BBYEFJWxtPCUtr3H2tERCSG+wa9J/RB7MAsGA1UdDwQEAwIBBjAPBgNVHRMBAf8E
BTADAQH/MIGPBgNVHSMEgYcwgYSAFJWxtPCUtr3H2tERCSG+wa9J/RB7oWmkZzBl
MQswCQYDVQQGEwJTRTEUMBIGA1UEChMLQWRkVHJ1c3QgQUIxHTAbBgNVBAsTFEFk
ZFRydXN0IFRUUCBOZXR3b3JrMSEwHwYDVQQDExhBZGRUcnVzdCBDbGFzcyAxIENB
IFJvb3SCAQEwDQYJKoZIhvcNAQEFBQADggEBACxtZBsfzQ3duQH6lmM0MkhHma6X
7f1yFqZzR1r0693p9db7RcwpiURdv0Y5PejuvE1Uhh4dbOMXJ0PhiVYrqW9yTkkz
43J8KiOavD7/KCrto/8cI7pDVwlnTUtiBi34/2ydYB7YHEt9tTEv2dB8Xfjea4MY
eDdXL+gzB2ffHsdrKpV2ro9Xo/D0UrSpUwjP4E/TelOL/bscVjby/rK25Xa71SJl
pz/+0WatC7xrmYbvP33zGDLKe8bjq2RGlfgmadlVg3sslgf/WSxEo8bl6ancoWOA
WiFeIc9TVPC6b4nbqKqVz4vjccweGyBECMB6tkD9xOQ14R0WHNC8K47Wcdk=
-----END CERTIFICATE-----

# Issuer: CN=AddTrust External CA Root O=AddTrust AB OU=AddTrust External TTP Network
# Subject: CN=AddTrust External CA Root O=AddTrust AB OU=AddTrust External TTP Network
# Label: "AddTrust External Root"
# Serial: 1
# MD5 Fingerprint: 1d:35:54:04:85:78:b0:3f:42:42:4d:bf:20:73:0a:3f
# SHA1 Fingerprint: 02:fa:f3:e2:91:43:54:68:60:78:57:69:4d:f5:e4:5b:68:85:18:68
# SHA256 Fingerprint: 68:7f:a4:51:38:22:78:ff:f0:c8:b1:1f:8d:43:d5:76:67:1c:6e:b2:bc:ea:b4:13:fb:83:d9:65:d0:6d:2f:f2
-----BEGIN CERTIFICATE-----
MIIENjCCAx6gAwIBAgIBATANBgkqhkiG9w0BAQUFADBvMQswCQYDVQQGEwJTRTEU
MBIGA1UEChMLQWRkVHJ1c3QgQUIxJjAkBgNVBAsTHUFkZFRydXN0IEV4dGVybmFs
IFRUUCBOZXR3b3JrMSIwIAYDVQQDExlBZGRUcnVzdCBFeHRlcm5hbCBDQSBSb290
MB4XDTAwMDUzMDEwNDgzOFoXDTIwMDUzMDEwNDgzOFowbzELMAkGA1UEBhMCU0Ux
FDASBgNVBAoTC0FkZFRydXN0IEFCMSYwJAYDVQQLEx1BZGRUcnVzdCBFeHRlcm5h
bCBUVFAgTmV0d29yazEiMCAGA1UEAxMZQWRkVHJ1c3QgRXh0ZXJuYWwgQ0EgUm9v
dDCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBALf3GjPm8gAELTngTlvt
H7xsD821+iO2zt6bETOXpClMfZOfvUq8k+0DGuOPz+VtUFrWlymUWoCwSXrbLpX9
uMq/NzgtHj6RQa1wVsfwTz/oMp50ysiQVOnGXw94nZpAPA6sYapeFI+eh6FqUNzX
mk6vBbOmcZSccbNQYArHE504B4YCqOmoaSYYkKtMsE8jqzpPhNjfzp/haW+710LX
a0Tkx63ubUFfclpxCDezeWWkWaCUN/cALw3CknLa0Dhy2xSoRcRdKn23tNbE7qzN
E0S3ySvdQwAl+mG5aWpYIxG3pzOPVnVZ9c0p10a3CitlttNCbxWyuHv77+ldU9U0
WicCAwEAAaOB3DCB2TAdBgNVHQ4EFgQUrb2YejS0Jvf6xCZU7wO94CTLVBowCwYD
VR0PBAQDAgEGMA8GA1UdEwEB/wQFMAMBAf8wgZkGA1UdIwSBkTCBjoAUrb2YejS0
Jvf6xCZU7wO94CTLVBqhc6RxMG8xCzAJBgNVBAYTAlNFMRQwEgYDVQQKEwtBZGRU
cnVzdCBBQjEmMCQGA1UECxMdQWRkVHJ1c3QgRXh0ZXJuYWwgVFRQIE5ldHdvcmsx
IjAgBgNVBAMTGUFkZFRydXN0IEV4dGVybmFsIENBIFJvb3SCAQEwDQYJKoZIhvcN
AQEFBQADggEBALCb4IUlwtYj4g+WBpKdQZic2YR5gdkeWxQHIzZlj7DYd7usQWxH
YINRsPkyPef89iYTx4AWpb9a/IfPeHmJIZriTAcKhjW88t5RxNKWt9x+Tu5w/Rw5
6wwCURQtjr0W4MHfRnXnJK3s9EK0hZNwEGe6nQY1ShjTK3rMUUKhemPR5ruhxSvC
Nr4TDea9Y355e6cJDUCrat2PisP29owaQgVR1EX1n6diIWgVIEM8med8vSTYqZEX
c4g/VhsxOBi0cQ+azcgOno4uG+GMmIPLHzHxREzGBHNJdmAPx/i9F4BrLunMTA5a
mnkPIAou1Z5jJh5VkpTYghdae9C8x49OhgQ=
-----END CERTIFICATE-----

# Issuer: CN=AddTrust Public CA Root O=AddTrust AB OU=AddTrust TTP Network
# Subject: CN=AddTrust Public CA Root O=AddTrust AB OU=AddTrust TTP Network
# Label: "AddTrust Public Services Root"
# Serial: 1
# MD5 Fingerprint: c1:62:3e:23:c5:82:73:9c:03:59:4b:2b:e9:77:49:7f
# SHA1 Fingerprint: 2a:b6:28:48:5e:78:fb:f3:ad:9e:79:10:dd:6b:df:99:72:2c:96:e5
# SHA256 Fingerprint: 07:91:ca:07:49:b2:07:82:aa:d3:c7:d7:bd:0c:df:c9:48:58:35:84:3e:b2:d7:99:60:09:ce:43:ab:6c:69:27
-----BEGIN CERTIFICATE-----
MIIEFTCCAv2gAwIBAgIBATANBgkqhkiG9w0BAQUFADBkMQswCQYDVQQGEwJTRTEU
MBIGA1UEChMLQWRkVHJ1c3QgQUIxHTAbBgNVBAsTFEFkZFRydXN0IFRUUCBOZXR3
b3JrMSAwHgYDVQQDExdBZGRUcnVzdCBQdWJsaWMgQ0EgUm9vdDAeFw0wMDA1MzAx
MDQxNTBaFw0yMDA1MzAxMDQxNTBaMGQxCzAJBgNVBAYTAlNFMRQwEgYDVQQKEwtB
ZGRUcnVzdCBBQjEdMBsGA1UECxMUQWRkVHJ1c3QgVFRQIE5ldHdvcmsxIDAeBgNV
BAMTF0FkZFRydXN0IFB1YmxpYyBDQSBSb290MIIBIjANBgkqhkiG9w0BAQEFAAOC
AQ8AMIIBCgKCAQEA6Rowj4OIFMEg2Dybjxt+A3S72mnTRqX4jsIMEZBRpS9mVEBV
6tsfSlbunyNu9DnLoblv8n75XYcmYZ4c+OLspoH4IcUkzBEMP9smcnrHAZcHF/nX
GCwwfQ56HmIexkvA/X1id9NEHif2P0tEs7c42TkfYNVRknMDtABp4/MUTu7R3AnP
dzRGULD4EfL+OHn3Bzn+UZKXC1sIXzSGAa2Il+tmzV7R/9x98oTaunet3IAIx6eH
1lWfl2royBFkuucZKT8Rs3iQhCBSWxHveNCD9tVIkNAwHM+A+WD+eeSI8t0A65RF
62WUaUC6wNW0uLp9BBGo6zEFlpROWCGOn9Bg/QIDAQABo4HRMIHOMB0GA1UdDgQW
BBSBPjfYkrAfd59ctKtzquf2NGAv+jALBgNVHQ8EBAMCAQYwDwYDVR0TAQH/BAUw
AwEB/zCBjgYDVR0jBIGGMIGDgBSBPjfYkrAfd59ctKtzquf2NGAv+qFopGYwZDEL
MAkGA1UEBhMCU0UxFDASBgNVBAoTC0FkZFRydXN0IEFCMR0wGwYDVQQLExRBZGRU
cnVzdCBUVFAgTmV0d29yazEgMB4GA1UEAxMXQWRkVHJ1c3QgUHVibGljIENBIFJv
b3SCAQEwDQYJKoZIhvcNAQEFBQADggEBAAP3FUr4JNojVhaTdt02KLmuG7jD8WS6
IBh4lSknVwW8fCr0uVFV2ocC3g8WFzH4qnkuCRO7r7IgGRLlk/lL+YPoRNWyQSW/
iHVv/xD8SlTQX/D67zZzfRs2RcYhbbQVuE7PnFylPVoAjgbjPGsye/Kf8Lb93/Ao
GEjwxrzQvzSAlsJKsW2Ox5BF3i9nrEUEo3rcVZLJR2bYGozH7ZxOmuASu7VqTITh
4SINhwBk/ox9Yjllpu9CtoAlEmEBqCQTcAARJl/6NVDFSMwGR+gn2HCNX2TmoUQm
XiLsks3/QppEIW1cxeMiHV9HEufOX1362KqxMy3ZdvJOOjMMK7MtkAY=
-----END CERTIFICATE-----

# Issuer: CN=AddTrust Qualified CA Root O=AddTrust AB OU=AddTrust TTP Network
# Subject: CN=AddTrust Qualified CA Root O=AddTrust AB OU=AddTrust TTP Network
# Label: "AddTrust Qualified Certificates Root"
# Serial: 1
# MD5 Fingerprint: 27:ec:39:47:cd:da:5a:af:e2:9a:01:65:21:a9:4c:bb
# SHA1 Fingerprint: 4d:23:78:ec:91:95:39:b5:00:7f:75:8f:03:3b:21:1e:c5:4d:8b:cf
# SHA256 Fingerprint: 80:95:21:08:05:db:4b:bc:35:5e:44:28:d8:fd:6e:c2:cd:e3:ab:5f:b9:7a:99:42:98:8e:b8:f4:dc:d0:60:16
-----BEGIN CERTIFICATE-----
MIIEHjCCAwagAwIBAgIBATANBgkqhkiG9w0BAQUFADBnMQswCQYDVQQGEwJTRTEU
MBIGA1UEChMLQWRkVHJ1c3QgQUIxHTAbBgNVBAsTFEFkZFRydXN0IFRUUCBOZXR3
b3JrMSMwIQYDVQQDExpBZGRUcnVzdCBRdWFsaWZpZWQgQ0EgUm9vdDAeFw0wMDA1
MzAxMDQ0NTBaFw0yMDA1MzAxMDQ0NTBaMGcxCzAJBgNVBAYTAlNFMRQwEgYDVQQK
EwtBZGRUcnVzdCBBQjEdMBsGA1UECxMUQWRkVHJ1c3QgVFRQIE5ldHdvcmsxIzAh
BgNVBAMTGkFkZFRydXN0IFF1YWxpZmllZCBDQSBSb290MIIBIjANBgkqhkiG9w0B
AQEFAAOCAQ8AMIIBCgKCAQEA5B6a/twJWoekn0e+EV+vhDTbYjx5eLfpMLXsDBwq
xBb/4Oxx64r1EW7tTw2R0hIYLUkVAcKkIhPHEWT/IhKauY5cLwjPcWqzZwFZ8V1G
87B4pfYOQnrjfxvM0PC3KP0q6p6zsLkEqv32x7SxuCqg+1jxGaBvcCV+PmlKfw8i
2O+tCBGaKZnhqkRFmhJePp1tUvznoD1oL/BLcHwTOK28FSXx1s6rosAx1i+f4P8U
WfyEk9mHfExUE+uf0S0R+Bg6Ot4l2ffTQO2kBhLEO+GRwVY18BTcZTYJbqukB8c1
0cIDMzZbdSZtQvESa0NvS3GU+jQd7RNuyoB/mC9suWXY6QIDAQABo4HUMIHRMB0G
A1UdDgQWBBQ5lYtii1zJ1IC6WA+XPxUIQ8yYpzALBgNVHQ8EBAMCAQYwDwYDVR0T
AQH/BAUwAwEB/zCBkQYDVR0jBIGJMIGGgBQ5lYtii1zJ1IC6WA+XPxUIQ8yYp6Fr
pGkwZzELMAkGA1UEBhMCU0UxFDASBgNVBAoTC0FkZFRydXN0IEFCMR0wGwYDVQQL
ExRBZGRUcnVzdCBUVFAgTmV0d29yazEjMCEGA1UEAxMaQWRkVHJ1c3QgUXVhbGlm
aWVkIENBIFJvb3SCAQEwDQYJKoZIhvcNAQEFBQADggEBABmrder4i2VhlRO6aQTv
hsoToMeqT2QbPxj2qC0sVY8FtzDqQmodwCVRLae/DLPt7wh/bDxGGuoYQ992zPlm
hpwsaPXpF/gxsxjE1kh9I0xowX67ARRvxdlu3rsEQmr49lx95dr6h+sNNVJn0J6X
dgWTP5XHAeZpVTh/EGGZyeNfpso+gmNIquIISD6q8rKFYqa0p9m9N5xotS1WfbC3
P6CxB9bpT9zeRXEwMn8bLgn5v1Kh7sKAPgZcLlVAwRv1cEWw3F369nJad9Jjzc9Y
iQBCYz95OdBEsIJuQRno3eDBiFrRHnGTHyQwdOUeqN48Jzd/g66ed8/wMLH/S5no
xqE=
-----END CERTIFICATE-----

# Issuer: CN=Entrust Root Certification Authority O=Entrust, Inc. OU=www.entrust.net/CPS is incorporated by reference/(c) 2006 Entrust, Inc.
# Subject: CN=Entrust Root Certification Authority O=Entrust, Inc. OU=www.entrust.net/CPS is incorporated by reference/(c) 2006 Entrust, Inc.
# Label: "Entrust Root Certification Authority"
# Serial: 1164660820
# MD5 Fingerprint: d6:a5:c3:ed:5d:dd:3e:00:c1:3d:87:92:1f:1d:3f:e4
# SHA1 Fingerprint: b3:1e:b1:b7:40:e3:6c:84:02:da:dc:37:d4:4d:f5:d4:67:49:52:f9
# SHA256 Fingerprint: 73:c1:76:43:4f:1b:c6:d5:ad:f4:5b:0e:76:e7:27:28:7c:8d:e5:76:16:c1:e6:e6:14:1a:2b:2c:bc:7d:8e:4c
-----BEGIN CERTIFICATE-----
MIIEkTCCA3mgAwIBAgIERWtQVDANBgkqhkiG9w0BAQUFADCBsDELMAkGA1UEBhMC
VVMxFjAUBgNVBAoTDUVudHJ1c3QsIEluYy4xOTA3BgNVBAsTMHd3dy5lbnRydXN0
Lm5ldC9DUFMgaXMgaW5jb3Jwb3JhdGVkIGJ5IHJlZmVyZW5jZTEfMB0GA1UECxMW
KGMpIDIwMDYgRW50cnVzdCwgSW5jLjEtMCsGA1UEAxMkRW50cnVzdCBSb290IENl
cnRpZmljYXRpb24gQXV0aG9yaXR5MB4XDTA2MTEyNzIwMjM0MloXDTI2MTEyNzIw
NTM0MlowgbAxCzAJBgNVBAYTAlVTMRYwFAYDVQQKEw1FbnRydXN0LCBJbmMuMTkw
NwYDVQQLEzB3d3cuZW50cnVzdC5uZXQvQ1BTIGlzIGluY29ycG9yYXRlZCBieSBy
ZWZlcmVuY2UxHzAdBgNVBAsTFihjKSAyMDA2IEVudHJ1c3QsIEluYy4xLTArBgNV
BAMTJEVudHJ1c3QgUm9vdCBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTCCASIwDQYJ
KoZIhvcNAQEBBQADggEPADCCAQoCggEBALaVtkNC+sZtKm9I35RMOVcF7sN5EUFo
Nu3s/poBj6E4KPz3EEZmLk0eGrEaTsbRwJWIsMn/MYszA9u3g3s+IIRe7bJWKKf4
4LlAcTfFy0cOlypowCKVYhXbR9n10Cv/gkvJrT7eTNuQgFA/CYqEAOwwCj0Yzfv9
KlmaI5UXLEWeH25DeW0MXJj+SKfFI0dcXv1u5x609mhF0YaDW6KKjbHjKYD+JXGI
rb68j6xSlkuqUY3kEzEZ6E5Nn9uss2rVvDlUccp6en+Q3X0dgNmBu1kmwhH+5pPi
94DkZfs0Nw4pgHBNrziGLp5/V6+eF67rHMsoIV+2HNjnogQi+dPa2MsCAwEAAaOB
sDCBrTAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB/zArBgNVHRAEJDAi
gA8yMDA2MTEyNzIwMjM0MlqBDzIwMjYxMTI3MjA1MzQyWjAfBgNVHSMEGDAWgBRo
kORnpKZTgMeGZqTx90tD+4S9bTAdBgNVHQ4EFgQUaJDkZ6SmU4DHhmak8fdLQ/uE
vW0wHQYJKoZIhvZ9B0EABBAwDhsIVjcuMTo0LjADAgSQMA0GCSqGSIb3DQEBBQUA
A4IBAQCT1DCw1wMgKtD5Y+iRDAUgqV8ZyntyTtSx29CW+1RaGSwMCPeyvIWonX9t
O1KzKtvn1ISMY/YPyyYBkVBs9F8U4pN0wBOeMDpQ47RgxRzwIkSNcUesyBrJ6Zua
AGAT/3B+XxFNSRuzFVJ7yVTav52Vr2ua2J7p8eRDjeIRRDq/r72DQnNSi6q7pynP
9WQcCk3RvKqsnyrQ/39/2n3qse0wJcGE2jTSW3iDVuycNsMm4hH2Z0kdkquM++v/
eu6FSqdQgPCnXEqULl8FmTxSQeDNtGPPAUO6nIPcj2A781q0tHuu2guQOHXvgR1m
0vdXcDazv/wor3ElhVsT/h5/WrQ8
-----END CERTIFICATE-----

# Issuer: O=RSA Security Inc OU=RSA Security 2048 V3
# Subject: O=RSA Security Inc OU=RSA Security 2048 V3
# Label: "RSA Security 2048 v3"
# Serial: 13297492616345471454730593562152402946
# MD5 Fingerprint: 77:0d:19:b1:21:fd:00:42:9c:3e:0c:a5:dd:0b:02:8e
# SHA1 Fingerprint: 25:01:90:19:cf:fb:d9:99:1c:b7:68:25:74:8d:94:5f:30:93:95:42
# SHA256 Fingerprint: af:8b:67:62:a1:e5:28:22:81:61:a9:5d:5c:55:9e:e2:66:27:8f:75:d7:9e:83:01:89:a5:03:50:6a:bd:6b:4c
-----BEGIN CERTIFICATE-----
MIIDYTCCAkmgAwIBAgIQCgEBAQAAAnwAAAAKAAAAAjANBgkqhkiG9w0BAQUFADA6
MRkwFwYDVQQKExBSU0EgU2VjdXJpdHkgSW5jMR0wGwYDVQQLExRSU0EgU2VjdXJp
dHkgMjA0OCBWMzAeFw0wMTAyMjIyMDM5MjNaFw0yNjAyMjIyMDM5MjNaMDoxGTAX
BgNVBAoTEFJTQSBTZWN1cml0eSBJbmMxHTAbBgNVBAsTFFJTQSBTZWN1cml0eSAy
MDQ4IFYzMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAt49VcdKA3Xtp
eafwGFAyPGJn9gqVB93mG/Oe2dJBVGutn3y+Gc37RqtBaB4Y6lXIL5F4iSj7Jylg
/9+PjDvJSZu1pJTOAeo+tWN7fyb9Gd3AIb2E0S1PRsNO3Ng3OTsor8udGuorryGl
wSMiuLgbWhOHV4PR8CDn6E8jQrAApX2J6elhc5SYcSa8LWrg903w8bYqODGBDSnh
AMFRD0xS+ARaqn1y07iHKrtjEAMqs6FPDVpeRrc9DvV07Jmf+T0kgYim3WBU6JU2
PcYJk5qjEoAAVZkZR73QpXzDuvsf9/UP+Ky5tfQ3mBMY3oVbtwyCO4dvlTlYMNpu
AWgXIszACwIDAQABo2MwYTAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIB
BjAfBgNVHSMEGDAWgBQHw1EwpKrpRa41JPr/JCwz0LGdjDAdBgNVHQ4EFgQUB8NR
MKSq6UWuNST6/yQsM9CxnYwwDQYJKoZIhvcNAQEFBQADggEBAF8+hnZuuDU8TjYc
HnmYv/3VEhF5Ug7uMYm83X/50cYVIeiKAVQNOvtUudZj1LGqlk2iQk3UUx+LEN5/
Zb5gEydxiKRz44Rj0aRV4VCT5hsOedBnvEbIvz8XDZXmxpBp3ue0L96VfdASPz0+
f00/FGj1EVDVwfSQpQgdMWD/YIwjVAqv/qFuxdF6Kmh4zx6CCiC0H63lhbJqaHVO
rSU3lIW+vaHU6rcMSzyd6BIA8F+sDeGscGNz9395nzIlQnQFgCi/vcEkllgVsRch
6YlL2weIZ/QVrXA+L02FO8K32/6YaCOJ4XQP3vTFhGMpG8zLB8kApKnXwiJPZ9d3
7CAFYd4=
-----END CERTIFICATE-----

# Issuer: CN=GeoTrust Global CA O=GeoTrust Inc.
# Subject: CN=GeoTrust Global CA O=GeoTrust Inc.
# Label: "GeoTrust Global CA"
# Serial: 144470
# MD5 Fingerprint: f7:75:ab:29:fb:51:4e:b7:77:5e:ff:05:3c:99:8e:f5
# SHA1 Fingerprint: de:28:f4:a4:ff:e5:b9:2f:a3:c5:03:d1:a3:49:a7:f9:96:2a:82:12
# SHA256 Fingerprint: ff:85:6a:2d:25:1d:cd:88:d3:66:56:f4:50:12:67:98:cf:ab:aa:de:40:79:9c:72:2d:e4:d2:b5:db:36:a7:3a
-----BEGIN CERTIFICATE-----
MIIDVDCCAjygAwIBAgIDAjRWMA0GCSqGSIb3DQEBBQUAMEIxCzAJBgNVBAYTAlVT
MRYwFAYDVQQKEw1HZW9UcnVzdCBJbmMuMRswGQYDVQQDExJHZW9UcnVzdCBHbG9i
YWwgQ0EwHhcNMDIwNTIxMDQwMDAwWhcNMjIwNTIxMDQwMDAwWjBCMQswCQYDVQQG
EwJVUzEWMBQGA1UEChMNR2VvVHJ1c3QgSW5jLjEbMBkGA1UEAxMSR2VvVHJ1c3Qg
R2xvYmFsIENBMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA2swYYzD9
9BcjGlZ+W988bDjkcbd4kdS8odhM+KhDtgPpTSEHCIjaWC9mOSm9BXiLnTjoBbdq
fnGk5sRgprDvgOSJKA+eJdbtg/OtppHHmMlCGDUUna2YRpIuT8rxh0PBFpVXLVDv
iS2Aelet8u5fa9IAjbkU+BQVNdnARqN7csiRv8lVK83Qlz6cJmTM386DGXHKTubU
1XupGc1V3sjs0l44U+VcT4wt/lAjNvxm5suOpDkZALeVAjmRCw7+OC7RHQWa9k0+
bw8HHa8sHo9gOeL6NlMTOdReJivbPagUvTLrGAMoUgRx5aszPeE4uwc2hGKceeoW
MPRfwCvocWvk+QIDAQABo1MwUTAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBTA
ephojYn7qwVkDBF9qn1luMrMTjAfBgNVHSMEGDAWgBTAephojYn7qwVkDBF9qn1l
uMrMTjANBgkqhkiG9w0BAQUFAAOCAQEANeMpauUvXVSOKVCUn5kaFOSPeCpilKIn
Z57QzxpeR+nBsqTP3UEaBU6bS+5Kb1VSsyShNwrrZHYqLizz/Tt1kL/6cdjHPTfS
tQWVYrmm3ok9Nns4d0iXrKYgjy6myQzCsplFAMfOEVEiIuCl6rYVSAlk6l5PdPcF
PseKUgzbFbS9bZvlxrFUaKnjaZC2mqUPuLk/IH2uSrW4nOQdtqvmlKXBx4Ot2/Un
hw4EbNX/3aBd7YdStysVAq45pmp06drE57xNNB6pXE0zX5IJL4hmXXeXxx12E6nV
5fEWCRE11azbJHFwLJhWC9kXtNHjUStedejV0NxPNO3CBWaAocvmMw==
-----END CERTIFICATE-----

# Issuer: CN=GeoTrust Global CA 2 O=GeoTrust Inc.
# Subject: CN=GeoTrust Global CA 2 O=GeoTrust Inc.
# Label: "GeoTrust Global CA 2"
# Serial: 1
# MD5 Fingerprint: 0e:40:a7:6c:de:03:5d:8f:d1:0f:e4:d1:8d:f9:6c:a9
# SHA1 Fingerprint: a9:e9:78:08:14:37:58:88:f2:05:19:b0:6d:2b:0d:2b:60:16:90:7d
# SHA256 Fingerprint: ca:2d:82:a0:86:77:07:2f:8a:b6:76:4f:f0:35:67:6c:fe:3e:5e:32:5e:01:21:72:df:3f:92:09:6d:b7:9b:85
-----BEGIN CERTIFICATE-----
MIIDZjCCAk6gAwIBAgIBATANBgkqhkiG9w0BAQUFADBEMQswCQYDVQQGEwJVUzEW
MBQGA1UEChMNR2VvVHJ1c3QgSW5jLjEdMBsGA1UEAxMUR2VvVHJ1c3QgR2xvYmFs
IENBIDIwHhcNMDQwMzA0MDUwMDAwWhcNMTkwMzA0MDUwMDAwWjBEMQswCQYDVQQG
EwJVUzEWMBQGA1UEChMNR2VvVHJ1c3QgSW5jLjEdMBsGA1UEAxMUR2VvVHJ1c3Qg
R2xvYmFsIENBIDIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDvPE1A
PRDfO1MA4Wf+lGAVPoWI8YkNkMgoI5kF6CsgncbzYEbYwbLVjDHZ3CB5JIG/NTL8
Y2nbsSpr7iFY8gjpeMtvy/wWUsiRxP89c96xPqfCfWbB9X5SJBri1WeR0IIQ13hL
TytCOb1kLUCgsBDTOEhGiKEMuzozKmKY+wCdE1l/bztyqu6mD4b5BWHqZ38MN5aL
5mkWRxHCJ1kDs6ZgwiFAVvqgx306E+PsV8ez1q6diYD3Aecs9pYrEw15LNnA5IZ7
S4wMcoKK+xfNAGw6EzywhIdLFnopsk/bHdQL82Y3vdj2V7teJHq4PIu5+pIaGoSe
2HSPqht/XvT+RSIhAgMBAAGjYzBhMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYE
FHE4NvICMVNHK266ZUapEBVYIAUJMB8GA1UdIwQYMBaAFHE4NvICMVNHK266ZUap
EBVYIAUJMA4GA1UdDwEB/wQEAwIBhjANBgkqhkiG9w0BAQUFAAOCAQEAA/e1K6td
EPx7srJerJsOflN4WT5CBP51o62sgU7XAotexC3IUnbHLB/8gTKY0UvGkpMzNTEv
/NgdRN3ggX+d6YvhZJFiCzkIjKx0nVnZellSlxG5FntvRdOW2TF9AjYPnDtuzywN
A0ZF66D0f0hExghAzN4bcLUprbqLOzRldRtxIR0sFAqwlpW41uryZfspuk/qkZN0
abby/+Ea0AzRdoXLiiW9l14sbxWZJue2Kf8i7MkCx1YAzUm5s2x7UwQa4qjJqhIF
I8LO57sEAszAR6LkxCkvW0VXiVHuPOtSCP8HNR6fNWpHSlaY0VqFH4z1Ir+rzoPz
4iIprn2DQKi6bA==
-----END CERTIFICATE-----

# Issuer: CN=GeoTrust Universal CA O=GeoTrust Inc.
# Subject: CN=GeoTrust Universal CA O=GeoTrust Inc.
# Label: "GeoTrust Universal CA"
# Serial: 1
# MD5 Fingerprint: 92:65:58:8b:a2:1a:31:72:73:68:5c:b4:a5:7a:07:48
# SHA1 Fingerprint: e6:21:f3:35:43:79:05:9a:4b:68:30:9d:8a:2f:74:22:15:87:ec:79
# SHA256 Fingerprint: a0:45:9b:9f:63:b2:25:59:f5:fa:5d:4c:6d:b3:f9:f7:2f:f1:93:42:03:35:78:f0:73:bf:1d:1b:46:cb:b9:12
-----BEGIN CERTIFICATE-----
MIIFaDCCA1CgAwIBAgIBATANBgkqhkiG9w0BAQUFADBFMQswCQYDVQQGEwJVUzEW
MBQGA1UEChMNR2VvVHJ1c3QgSW5jLjEeMBwGA1UEAxMVR2VvVHJ1c3QgVW5pdmVy
c2FsIENBMB4XDTA0MDMwNDA1MDAwMFoXDTI5MDMwNDA1MDAwMFowRTELMAkGA1UE
BhMCVVMxFjAUBgNVBAoTDUdlb1RydXN0IEluYy4xHjAcBgNVBAMTFUdlb1RydXN0
IFVuaXZlcnNhbCBDQTCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIBAKYV
VaCjxuAfjJ0hUNfBvitbtaSeodlyWL0AG0y/YckUHUWCq8YdgNY96xCcOq9tJPi8
cQGeBvV8Xx7BDlXKg5pZMK4ZyzBIle0iN430SppyZj6tlcDgFgDgEB8rMQ7XlFTT
QjOgNB0eRXbdT8oYN+yFFXoZCPzVx5zw8qkuEKmS5j1YPakWaDwvdSEYfyh3peFh
F7em6fgemdtzbvQKoiFs7tqqhZJmr/Z6a4LauiIINQ/PQvE1+mrufislzDoR5G2v
c7J2Ha3QsnhnGqQ5HFELZ1aD/ThdDc7d8Lsrlh/eezJS/R27tQahsiFepdaVaH/w
mZ7cRQg+59IJDTWU3YBOU5fXtQlEIGQWFwMCTFMNaN7VqnJNk22CDtucvc+081xd
VHppCZbW2xHBjXWotM85yM48vCR85mLK4b19p71XZQvk/iXttmkQ3CgaRr0BHdCX
teGYO8A3ZNY9lO4L4fUorgtWv3GLIylBjobFS1J72HGrH4oVpjuDWtdYAVHGTEHZ
f9hBZ3KiKN9gg6meyHv8U3NyWfWTehd2Ds735VzZC1U0oqpbtWpU5xPKV+yXbfRe
Bi9Fi1jUIxaS5BZuKGNZMN9QAZxjiRqf2xeUgnA3wySemkfWWspOqGmJch+RbNt+
nhutxx9z3SxPGWX9f5NAEC7S8O08ni4oPmkmM8V7AgMBAAGjYzBhMA8GA1UdEwEB
/wQFMAMBAf8wHQYDVR0OBBYEFNq7LqqwDLiIJlF0XG0D08DYj3rWMB8GA1UdIwQY
MBaAFNq7LqqwDLiIJlF0XG0D08DYj3rWMA4GA1UdDwEB/wQEAwIBhjANBgkqhkiG
9w0BAQUFAAOCAgEAMXjmx7XfuJRAyXHEqDXsRh3ChfMoWIawC/yOsjmPRFWrZIRc
aanQmjg8+uUfNeVE44B5lGiku8SfPeE0zTBGi1QrlaXv9z+ZhP015s8xxtxqv6fX
IwjhmF7DWgh2qaavdy+3YL1ERmrvl/9zlcGO6JP7/TG37FcREUWbMPEaiDnBTzyn
ANXH/KttgCJwpQzgXQQpAvvLoJHRfNbDflDVnVi+QTjruXU8FdmbyUqDWcDaU/0z
uzYYm4UPFd3uLax2k7nZAY1IEKj79TiG8dsKxr2EoyNB3tZ3b4XUhRxQ4K5RirqN
Pnbiucon8l+f725ZDQbYKxek0nxru18UGkiPGkzns0ccjkxFKyDuSN/n3QmOGKja
QI2SJhFTYXNd673nxE0pN2HrrDktZy4W1vUAg4WhzH92xH3kt0tm7wNFYGm2DFKW
koRepqO1pD4r2czYG0eq8kTaT/kD6PAUyz/zg97QwVTjt+gKN02LIFkDMBmhLMi9
ER/frslKxfMnZmaGrGiR/9nmUxwPi1xpZQomyB40w11Re9epnAahNt3ViZS82eQt
DF4JbAiXfKM9fJP/P6EUp8+1Xevb2xzEdt+Iub1FBZUbrvxGakyvSOPOrg/Sfuvm
bJxPgWp6ZKy7PtXny3YuxadIwVyQD8vIP/rmMuGNG2+k5o7Y+SlIis5z/iw=
-----END CERTIFICATE-----

# Issuer: CN=GeoTrust Universal CA 2 O=GeoTrust Inc.
# Subject: CN=GeoTrust Universal CA 2 O=GeoTrust Inc.
# Label: "GeoTrust Universal CA 2"
# Serial: 1
# MD5 Fingerprint: 34:fc:b8:d0:36:db:9e:14:b3:c2:f2:db:8f:e4:94:c7
# SHA1 Fingerprint: 37:9a:19:7b:41:85:45:35:0c:a6:03:69:f3:3c:2e:af:47:4f:20:79
# SHA256 Fingerprint: a0:23:4f:3b:c8:52:7c:a5:62:8e:ec:81:ad:5d:69:89:5d:a5:68:0d:c9:1d:1c:b8:47:7f:33:f8:78:b9:5b:0b
-----BEGIN CERTIFICATE-----
MIIFbDCCA1SgAwIBAgIBATANBgkqhkiG9w0BAQUFADBHMQswCQYDVQQGEwJVUzEW
MBQGA1UEChMNR2VvVHJ1c3QgSW5jLjEgMB4GA1UEAxMXR2VvVHJ1c3QgVW5pdmVy
c2FsIENBIDIwHhcNMDQwMzA0MDUwMDAwWhcNMjkwMzA0MDUwMDAwWjBHMQswCQYD
VQQGEwJVUzEWMBQGA1UEChMNR2VvVHJ1c3QgSW5jLjEgMB4GA1UEAxMXR2VvVHJ1
c3QgVW5pdmVyc2FsIENBIDIwggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoIC
AQCzVFLByT7y2dyxUxpZKeexw0Uo5dfR7cXFS6GqdHtXr0om/Nj1XqduGdt0DE81
WzILAePb63p3NeqqWuDW6KFXlPCQo3RWlEQwAx5cTiuFJnSCegx2oG9NzkEtoBUG
FF+3Qs17j1hhNNwqCPkuwwGmIkQcTAeC5lvO0Ep8BNMZcyfwqph/Lq9O64ceJHdq
XbboW0W63MOhBW9Wjo8QJqVJwy7XQYci4E+GymC16qFjwAGXEHm9ADwSbSsVsaxL
se4YuU6W3Nx2/zu+z18DwPw76L5GG//aQMJS9/7jOvdqdzXQ2o3rXhhqMcceujwb
KNZrVMaqW9eiLBsZzKIC9ptZvTdrhrVtgrrY6slWvKk2WP0+GfPtDCapkzj4T8Fd
IgbQl+rhrcZV4IErKIM6+vR7IVEAvlI4zs1meaj0gVbi0IMJR1FbUGrP20gaXT73
y/Zl92zxlfgCOzJWgjl6W70viRu/obTo/3+NjN8D8WBOWBFM66M/ECuDmgFz2ZRt
hAAnZqzwcEAJQpKtT5MNYQlRJNiS1QuUYbKHsu3/mjX/hVTK7URDrBs8FmtISgoc
QIgfksILAAX/8sgCSqSqqcyZlpwvWOB94b67B9xfBHJcMTTD7F8t4D1kkCLm0ey4
Lt1ZrtmhN79UNdxzMk+MBB4zsslG8dhcyFVQyWi9qLo2CQIDAQABo2MwYTAPBgNV
HRMBAf8EBTADAQH/MB0GA1UdDgQWBBR281Xh+qQ2+/CfXGJx7Tz0RzgQKzAfBgNV
HSMEGDAWgBR281Xh+qQ2+/CfXGJx7Tz0RzgQKzAOBgNVHQ8BAf8EBAMCAYYwDQYJ
KoZIhvcNAQEFBQADggIBAGbBxiPz2eAubl/oz66wsCVNK/g7WJtAJDday6sWSf+z
dXkzoS9tcBc0kf5nfo/sm+VegqlVHy/c1FEHEv6sFj4sNcZj/NwQ6w2jqtB8zNHQ
L1EuxBRa3ugZ4T7GzKQp5y6EqgYweHZUcyiYWTjgAA1i00J9IZ+uPTqM1fp3DRgr
Fg5fNuH8KrUwJM/gYwx7WBr+mbpCErGR9Hxo4sjoryzqyX6uuyo9DRXcNJW2GHSo
ag/HtPQTxORb7QrSpJdMKu0vbBKJPfEncKpqA1Ihn0CoZ1Dy81of398j9tx4TuaY
T1U6U+Pv8vSfx3zYWK8pIpe44L2RLrB27FcRz+8pRPPphXpgY+RdM4kX2TGq2tbz
GDVyz4crL2MjhF2EjD9XoIj8mZEoJmmZ1I+XRL6O1UixpCgp8RW04eWe3fiPpm8m
1wk8OhwRDqZsN/etRIcsKMfYdIKz0G9KV7s1KSegi+ghp4dkNl3M2Basx7InQJJV
OCiNUW7dFGdTbHFcJoRNdVq2fmBWqU2t+5sel/MN2dKXVHfaPRK34B7vCAas+YWH
6aLcr34YEoP9VhdBLtUpgn2Z9DH2canPLAEnpQW5qrJITirvn5NSUZU8UnOOVkwX
QMAJKOSLakhT2+zNVVXxxvjpoixMptEmX36vWkzaH6byHCx+rgIW0lbQL1dTR+iS
-----END CERTIFICATE-----

# Issuer: CN=America Online Root Certification Authority 1 O=America Online Inc.
# Subject: CN=America Online Root Certification Authority 1 O=America Online Inc.
# Label: "America Online Root Certification Authority 1"
# Serial: 1
# MD5 Fingerprint: 14:f1:08:ad:9d:fa:64:e2:89:e7:1c:cf:a8:ad:7d:5e
# SHA1 Fingerprint: 39:21:c1:15:c1:5d:0e:ca:5c:cb:5b:c4:f0:7d:21:d8:05:0b:56:6a
# SHA256 Fingerprint: 77:40:73:12:c6:3a:15:3d:5b:c0:0b:4e:51:75:9c:df:da:c2:37:dc:2a:33:b6:79:46:e9:8e:9b:fa:68:0a:e3
-----BEGIN CERTIFICATE-----
MIIDpDCCAoygAwIBAgIBATANBgkqhkiG9w0BAQUFADBjMQswCQYDVQQGEwJVUzEc
MBoGA1UEChMTQW1lcmljYSBPbmxpbmUgSW5jLjE2MDQGA1UEAxMtQW1lcmljYSBP
bmxpbmUgUm9vdCBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eSAxMB4XDTAyMDUyODA2
MDAwMFoXDTM3MTExOTIwNDMwMFowYzELMAkGA1UEBhMCVVMxHDAaBgNVBAoTE0Ft
ZXJpY2EgT25saW5lIEluYy4xNjA0BgNVBAMTLUFtZXJpY2EgT25saW5lIFJvb3Qg
Q2VydGlmaWNhdGlvbiBBdXRob3JpdHkgMTCCASIwDQYJKoZIhvcNAQEBBQADggEP
ADCCAQoCggEBAKgv6KRpBgNHw+kqmP8ZonCaxlCyfqXfaE0bfA+2l2h9LaaLl+lk
hsmj76CGv2BlnEtUiMJIxUo5vxTjWVXlGbR0yLQFOVwWpeKVBeASrlmLojNoWBym
1BW32J/X3HGrfpq/m44zDyL9Hy7nBzbvYjnF3cu6JRQj3gzGPTzOggjmZj7aUTsW
OqMFf6Dch9Wc/HKpoH145LcxVR5lu9RhsCFg7RAycsWSJR74kEoYeEfffjA3PlAb
2xzTa5qGUwew76wGePiEmf4hjUyAtgyC9mZweRrTT6PP8c9GsEsPPt2IYriMqQko
O3rHl+Ee5fSfwMCuJKDIodkP1nsmgmkyPacCAwEAAaNjMGEwDwYDVR0TAQH/BAUw
AwEB/zAdBgNVHQ4EFgQUAK3Zo/Z59m50qX8zPYEX10zPM94wHwYDVR0jBBgwFoAU
AK3Zo/Z59m50qX8zPYEX10zPM94wDgYDVR0PAQH/BAQDAgGGMA0GCSqGSIb3DQEB
BQUAA4IBAQB8itEfGDeC4Liwo+1WlchiYZwFos3CYiZhzRAW18y0ZTTQEYqtqKkF
Zu90821fnZmv9ov761KyBZiibyrFVL0lvV+uyIbqRizBs73B6UlwGBaXCBOMIOAb
LjpHyx7kADCVW/RFo8AasAFOq73AI25jP4BKxQft3OJvx8Fi8eNy1gTIdGcL+oir
oQHIb/AUr9KZzVGTfu0uOMe9zkZQPXLjeSWdm4grECDdpbgyn43gKd8hdIaC2y+C
MMbHNYaz+ZZfRtsMRf3zUMNvxsNIrUam4SdHCh0Om7bCd39j8uB9Gr784N/Xx6ds
sPmuujz9dLQR6FgNgLzTqIA6me11zEZ7
-----END CERTIFICATE-----

# Issuer: CN=America Online Root Certification Authority 2 O=America Online Inc.
# Subject: CN=America Online Root Certification Authority 2 O=America Online Inc.
# Label: "America Online Root Certification Authority 2"
# Serial: 1
# MD5 Fingerprint: d6:ed:3c:ca:e2:66:0f:af:10:43:0d:77:9b:04:09:bf
# SHA1 Fingerprint: 85:b5:ff:67:9b:0c:79:96:1f:c8:6e:44:22:00:46:13:db:17:92:84
# SHA256 Fingerprint: 7d:3b:46:5a:60:14:e5:26:c0:af:fc:ee:21:27:d2:31:17:27:ad:81:1c:26:84:2d:00:6a:f3:73:06:cc:80:bd
-----BEGIN CERTIFICATE-----
MIIFpDCCA4ygAwIBAgIBATANBgkqhkiG9w0BAQUFADBjMQswCQYDVQQGEwJVUzEc
MBoGA1UEChMTQW1lcmljYSBPbmxpbmUgSW5jLjE2MDQGA1UEAxMtQW1lcmljYSBP
bmxpbmUgUm9vdCBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eSAyMB4XDTAyMDUyODA2
MDAwMFoXDTM3MDkyOTE0MDgwMFowYzELMAkGA1UEBhMCVVMxHDAaBgNVBAoTE0Ft
ZXJpY2EgT25saW5lIEluYy4xNjA0BgNVBAMTLUFtZXJpY2EgT25saW5lIFJvb3Qg
Q2VydGlmaWNhdGlvbiBBdXRob3JpdHkgMjCCAiIwDQYJKoZIhvcNAQEBBQADggIP
ADCCAgoCggIBAMxBRR3pPU0Q9oyxQcngXssNt79Hc9PwVU3dxgz6sWYFas14tNwC
206B89enfHG8dWOgXeMHDEjsJcQDIPT/DjsS/5uN4cbVG7RtIuOx238hZK+GvFci
KtZHgVdEglZTvYYUAQv8f3SkWq7xuhG1m1hagLQ3eAkzfDJHA1zEpYNI9FdWboE2
JxhP7JsowtS013wMPgwr38oE18aO6lhOqKSlGBxsRZijQdEt0sdtjRnxrXm3gT+9
BoInLRBYBbV4Bbkv2wxrkJB+FFk4u5QkE+XRnRTf04JNRvCAOVIyD+OEsnpD8l7e
Xz8d3eOyG6ChKiMDbi4BFYdcpnV1x5dhvt6G3NRI270qv0pV2uh9UPu0gBe4lL8B
PeraunzgWGcXuVjgiIZGZ2ydEEdYMtA1fHkqkKJaEBEjNa0vzORKW6fIJ/KD3l67
Xnfn6KVuY8INXWHQjNJsWiEOyiijzirplcdIz5ZvHZIlyMbGwcEMBawmxNJ10uEq
Z8A9W6Wa6897GqidFEXlD6CaZd4vKL3Ob5Rmg0gp2OpljK+T2WSfVVcmv2/LNzGZ
o2C7HK2JNDJiuEMhBnIMoVxtRsX6Kc8w3onccVvdtjc+31D1uAclJuW8tf48ArO3
+L5DwYcRlJ4jbBeKuIonDFRH8KmzwICMoCfrHRnjB453cMor9H124HhnAgMBAAGj
YzBhMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFE1FwWg4u3OpaaEg5+31IqEj
FNeeMB8GA1UdIwQYMBaAFE1FwWg4u3OpaaEg5+31IqEjFNeeMA4GA1UdDwEB/wQE
AwIBhjANBgkqhkiG9w0BAQUFAAOCAgEAZ2sGuV9FOypLM7PmG2tZTiLMubekJcmn
xPBUlgtk87FYT15R/LKXeydlwuXK5w0MJXti4/qftIe3RUavg6WXSIylvfEWK5t2
LHo1YGwRgJfMqZJS5ivmae2p+DYtLHe/YUjRYwu5W1LtGLBDQiKmsXeu3mnFzccc
obGlHBD7GL4acN3Bkku+KVqdPzW+5X1R+FXgJXUjhx5c3LqdsKyzadsXg8n33gy8
CNyRnqjQ1xU3c6U1uPx+xURABsPr+CKAXEfOAuMRn0T//ZoyzH1kUQ7rVyZ2OuMe
IjzCpjbdGe+n/BLzJsBZMYVMnNjP36TMzCmT/5RtdlwTCJfy7aULTd3oyWgOZtMA
DjMSW7yV5TKQqLPGbIOtd+6Lfn6xqavT4fG2wLHqiMDn05DpKJKUe2h7lyoKZy2F
AjgQ5ANh1NolNscIWC2hp1GvMApJ9aZphwctREZ2jirlmjvXGKL8nDgQzMY70rUX
Om/9riW99XJZZLF0KjhfGEzfz3EEWjbUvy+ZnOjZurGV5gJLIaFb1cFPj65pbVPb
AZO1XB4Y3WRayhgoPmMEEf0cjQAPuDffZ4qdZqkCapH/E8ovXYO8h5Ns3CRRFgQl
Zvqz2cK6Kb6aSDiCmfS/O0oxGfm/jiEzFMpPVF/7zvuPcX/9XhmgD0uRuMRUvAaw
RY8mkaKO/qk=
-----END CERTIFICATE-----

# Issuer: CN=Visa eCommerce Root O=VISA OU=Visa International Service Association
# Subject: CN=Visa eCommerce Root O=VISA OU=Visa International Service Association
# Label: "Visa eCommerce Root"
# Serial: 25952180776285836048024890241505565794
# MD5 Fingerprint: fc:11:b8:d8:08:93:30:00:6d:23:f9:7e:eb:52:1e:02
# SHA1 Fingerprint: 70:17:9b:86:8c:00:a4:fa:60:91:52:22:3f:9f:3e:32:bd:e0:05:62
# SHA256 Fingerprint: 69:fa:c9:bd:55:fb:0a:c7:8d:53:bb:ee:5c:f1:d5:97:98:9f:d0:aa:ab:20:a2:51:51:bd:f1:73:3e:e7:d1:22
-----BEGIN CERTIFICATE-----
MIIDojCCAoqgAwIBAgIQE4Y1TR0/BvLB+WUF1ZAcYjANBgkqhkiG9w0BAQUFADBr
MQswCQYDVQQGEwJVUzENMAsGA1UEChMEVklTQTEvMC0GA1UECxMmVmlzYSBJbnRl
cm5hdGlvbmFsIFNlcnZpY2UgQXNzb2NpYXRpb24xHDAaBgNVBAMTE1Zpc2EgZUNv
bW1lcmNlIFJvb3QwHhcNMDIwNjI2MDIxODM2WhcNMjIwNjI0MDAxNjEyWjBrMQsw
CQYDVQQGEwJVUzENMAsGA1UEChMEVklTQTEvMC0GA1UECxMmVmlzYSBJbnRlcm5h
dGlvbmFsIFNlcnZpY2UgQXNzb2NpYXRpb24xHDAaBgNVBAMTE1Zpc2EgZUNvbW1l
cmNlIFJvb3QwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCvV95WHm6h
2mCxlCfLF9sHP4CFT8icttD0b0/Pmdjh28JIXDqsOTPHH2qLJj0rNfVIsZHBAk4E
lpF7sDPwsRROEW+1QK8bRaVK7362rPKgH1g/EkZgPI2h4H3PVz4zHvtH8aoVlwdV
ZqW1LS7YgFmypw23RuwhY/81q6UCzyr0TP579ZRdhE2o8mCP2w4lPJ9zcc+U30rq
299yOIzzlr3xF7zSujtFWsan9sYXiwGd/BmoKoMWuDpI/k4+oKsGGelT84ATB+0t
vz8KPFUgOSwsAGl0lUq8ILKpeeUYiZGo3BxN77t+Nwtd/jmliFKMAGzsGHxBvfaL
dXe6YJ2E5/4tAgMBAAGjQjBAMA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQD
AgEGMB0GA1UdDgQWBBQVOIMPPyw/cDMezUb+B4wg4NfDtzANBgkqhkiG9w0BAQUF
AAOCAQEAX/FBfXxcCLkr4NWSR/pnXKUTwwMhmytMiUbPWU3J/qVAtmPN3XEolWcR
zCSs00Rsca4BIGsDoo8Ytyk6feUWYFN4PMCvFYP3j1IzJL1kk5fui/fbGKhtcbP3
LBfQdCVp9/5rPJS+TUtBjE7ic9DjkCJzQ83z7+pzzkWKsKZJ/0x9nXGIxHYdkFsd
7v3M9+79YKWxehZx0RbQfBI8bGmX265fOZpwLwU8GUYEmSA20GBuYQa7FkKMcPcw
++DbZqMAAb3mLNqRX6BGi01qnD093QVG/na/oAo85ADmJ7f/hC3euiInlhBx6yLt
398znM/jra6O1I7mT1GvFpLgXPYHDw==
-----END CERTIFICATE-----

# Issuer: CN=Certum CA O=Unizeto Sp. z o.o.
# Subject: CN=Certum CA O=Unizeto Sp. z o.o.
# Label: "Certum Root CA"
# Serial: 65568
# MD5 Fingerprint: 2c:8f:9f:66:1d:18:90:b1:47:26:9d:8e:86:82:8c:a9
# SHA1 Fingerprint: 62:52:dc:40:f7:11:43:a2:2f:de:9e:f7:34:8e:06:42:51:b1:81:18
# SHA256 Fingerprint: d8:e0:fe:bc:1d:b2:e3:8d:00:94:0f:37:d2:7d:41:34:4d:99:3e:73:4b:99:d5:65:6d:97:78:d4:d8:14:36:24
-----BEGIN CERTIFICATE-----
MIIDDDCCAfSgAwIBAgIDAQAgMA0GCSqGSIb3DQEBBQUAMD4xCzAJBgNVBAYTAlBM
MRswGQYDVQQKExJVbml6ZXRvIFNwLiB6IG8uby4xEjAQBgNVBAMTCUNlcnR1bSBD
QTAeFw0wMjA2MTExMDQ2MzlaFw0yNzA2MTExMDQ2MzlaMD4xCzAJBgNVBAYTAlBM
MRswGQYDVQQKExJVbml6ZXRvIFNwLiB6IG8uby4xEjAQBgNVBAMTCUNlcnR1bSBD
QTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAM6xwS7TT3zNJc4YPk/E
jG+AanPIW1H4m9LcuwBcsaD8dQPugfCI7iNS6eYVM42sLQnFdvkrOYCJ5JdLkKWo
ePhzQ3ukYbDYWMzhbGZ+nPMJXlVjhNWo7/OxLjBos8Q82KxujZlakE403Daaj4GI
ULdtlkIJ89eVgw1BS7Bqa/j8D35in2fE7SZfECYPCE/wpFcozo+47UX2bu4lXapu
Ob7kky/ZR6By6/qmW6/KUz/iDsaWVhFu9+lmqSbYf5VT7QqFiLpPKaVCjF62/IUg
AKpoC6EahQGcxEZjgoi2IrHu/qpGWX7PNSzVttpd90gzFFS269lvzs2I1qsb2pY7
HVkCAwEAAaMTMBEwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQUFAAOCAQEA
uI3O7+cUus/usESSbLQ5PqKEbq24IXfS1HeCh+YgQYHu4vgRt2PRFze+GXYkHAQa
TOs9qmdvLdTN/mUxcMUbpgIKumB7bVjCmkn+YzILa+M6wKyrO7Do0wlRjBCDxjTg
xSvgGrZgFCdsMneMvLJymM/NzD+5yCRCFNZX/OYmQ6kd5YCQzgNUKD73P9P4Te1q
CjqTE5s7FCMTY5w/0YcneeVMUeMBrYVdGjux1XMQpNPyvG5k9VpWkKjHDkx0Dy5x
O/fIR/RpbxXyEV6DHpx8Uq79AtoSqFlnGNu8cN2bsWntgM6JQEhqDjXKKWYVIZQs
6GAqm4VKQPNriiTsBhYscw==
-----END CERTIFICATE-----

# Issuer: CN=AAA Certificate Services O=Comodo CA Limited
# Subject: CN=AAA Certificate Services O=Comodo CA Limited
# Label: "Comodo AAA Services root"
# Serial: 1
# MD5 Fingerprint: 49:79:04:b0:eb:87:19:ac:47:b0:bc:11:51:9b:74:d0
# SHA1 Fingerprint: d1:eb:23:a4:6d:17:d6:8f:d9:25:64:c2:f1:f1:60:17:64:d8:e3:49
# SHA256 Fingerprint: d7:a7:a0:fb:5d:7e:27:31:d7:71:e9:48:4e:bc:de:f7:1d:5f:0c:3e:0a:29:48:78:2b:c8:3e:e0:ea:69:9e:f4
-----BEGIN CERTIFICATE-----
MIIEMjCCAxqgAwIBAgIBATANBgkqhkiG9w0BAQUFADB7MQswCQYDVQQGEwJHQjEb
MBkGA1UECAwSR3JlYXRlciBNYW5jaGVzdGVyMRAwDgYDVQQHDAdTYWxmb3JkMRow
GAYDVQQKDBFDb21vZG8gQ0EgTGltaXRlZDEhMB8GA1UEAwwYQUFBIENlcnRpZmlj
YXRlIFNlcnZpY2VzMB4XDTA0MDEwMTAwMDAwMFoXDTI4MTIzMTIzNTk1OVowezEL
MAkGA1UEBhMCR0IxGzAZBgNVBAgMEkdyZWF0ZXIgTWFuY2hlc3RlcjEQMA4GA1UE
BwwHU2FsZm9yZDEaMBgGA1UECgwRQ29tb2RvIENBIExpbWl0ZWQxITAfBgNVBAMM
GEFBQSBDZXJ0aWZpY2F0ZSBTZXJ2aWNlczCCASIwDQYJKoZIhvcNAQEBBQADggEP
ADCCAQoCggEBAL5AnfRu4ep2hxxNRUSOvkbIgwadwSr+GB+O5AL686tdUIoWMQua
BtDFcCLNSS1UY8y2bmhGC1Pqy0wkwLxyTurxFa70VJoSCsN6sjNg4tqJVfMiWPPe
3M/vg4aijJRPn2jymJBGhCfHdr/jzDUsi14HZGWCwEiwqJH5YZ92IFCokcdmtet4
YgNW8IoaE+oxox6gmf049vYnMlhvB/VruPsUK6+3qszWY19zjNoFmag4qMsXeDZR
rOme9Hg6jc8P2ULimAyrL58OAd7vn5lJ8S3frHRNG5i1R8XlKdH5kBjHYpy+g8cm
ez6KJcfA3Z3mNWgQIJ2P2N7Sw4ScDV7oL8kCAwEAAaOBwDCBvTAdBgNVHQ4EFgQU
oBEKIz6W8Qfs4q8p74Klf9AwpLQwDgYDVR0PAQH/BAQDAgEGMA8GA1UdEwEB/wQF
MAMBAf8wewYDVR0fBHQwcjA4oDagNIYyaHR0cDovL2NybC5jb21vZG9jYS5jb20v
QUFBQ2VydGlmaWNhdGVTZXJ2aWNlcy5jcmwwNqA0oDKGMGh0dHA6Ly9jcmwuY29t
b2RvLm5ldC9BQUFDZXJ0aWZpY2F0ZVNlcnZpY2VzLmNybDANBgkqhkiG9w0BAQUF
AAOCAQEACFb8AvCb6P+k+tZ7xkSAzk/ExfYAWMymtrwUSWgEdujm7l3sAg9g1o1Q
GE8mTgHj5rCl7r+8dFRBv/38ErjHT1r0iWAFf2C3BUrz9vHCv8S5dIa2LX1rzNLz
Rt0vxuBqw8M0Ayx9lt1awg6nCpnBBYurDC/zXDrPbDdVCYfeU0BsWO/8tqtlbgT2
G9w84FoVxp7Z8VlIMCFlA2zs6SFz7JsDoeA3raAVGI/6ugLOpyypEBMs1OUIJqsi
l2D4kF501KKaU73yqWjgom7C12yxow+ev+to51byrvLjKzg6CYG1a4XXvi3tPxq3
smPi9WIsgtRqAEFQ8TmDn5XpNpaYbg==
-----END CERTIFICATE-----

# Issuer: CN=Secure Certificate Services O=Comodo CA Limited
# Subject: CN=Secure Certificate Services O=Comodo CA Limited
# Label: "Comodo Secure Services root"
# Serial: 1
# MD5 Fingerprint: d3:d9:bd:ae:9f:ac:67:24:b3:c8:1b:52:e1:b9:a9:bd
# SHA1 Fingerprint: 4a:65:d5:f4:1d:ef:39:b8:b8:90:4a:4a:d3:64:81:33:cf:c7:a1:d1
# SHA256 Fingerprint: bd:81:ce:3b:4f:65:91:d1:1a:67:b5:fc:7a:47:fd:ef:25:52:1b:f9:aa:4e:18:b9:e3:df:2e:34:a7:80:3b:e8
-----BEGIN CERTIFICATE-----
MIIEPzCCAyegAwIBAgIBATANBgkqhkiG9w0BAQUFADB+MQswCQYDVQQGEwJHQjEb
MBkGA1UECAwSR3JlYXRlciBNYW5jaGVzdGVyMRAwDgYDVQQHDAdTYWxmb3JkMRow
GAYDVQQKDBFDb21vZG8gQ0EgTGltaXRlZDEkMCIGA1UEAwwbU2VjdXJlIENlcnRp
ZmljYXRlIFNlcnZpY2VzMB4XDTA0MDEwMTAwMDAwMFoXDTI4MTIzMTIzNTk1OVow
fjELMAkGA1UEBhMCR0IxGzAZBgNVBAgMEkdyZWF0ZXIgTWFuY2hlc3RlcjEQMA4G
A1UEBwwHU2FsZm9yZDEaMBgGA1UECgwRQ29tb2RvIENBIExpbWl0ZWQxJDAiBgNV
BAMMG1NlY3VyZSBDZXJ0aWZpY2F0ZSBTZXJ2aWNlczCCASIwDQYJKoZIhvcNAQEB
BQADggEPADCCAQoCggEBAMBxM4KK0HDrc4eCQNUd5MvJDkKQ+d40uaG6EfQlhfPM
cm3ye5drswfxdySRXyWP9nQ95IDC+DwN879A6vfIUtFyb+/Iq0G4bi4XKpVpDM3S
HpR7LZQdqnXXs5jLrLxkU0C8j6ysNstcrbvd4JQX7NFc0L/vpZXJkMWwrPsbQ996
CF23uPJAGysnnlDOXmWCiIxe004MeuoIkbY2qitC++rCoznl2yY4rYsK7hljxxwk
3wN42ubqwUcaCwtGCd0C/N7Lh1/XMGNooa7cMqG6vv5Eq2i2pRcV/b3Vp6ea5EQz
6YiO/O1R65NxTq0B50SOqy3LqP4BSUjwwN3HaNiS/j0CAwEAAaOBxzCBxDAdBgNV
HQ4EFgQUPNiTiMLAggnMAZkGkyDpnnAJY08wDgYDVR0PAQH/BAQDAgEGMA8GA1Ud
EwEB/wQFMAMBAf8wgYEGA1UdHwR6MHgwO6A5oDeGNWh0dHA6Ly9jcmwuY29tb2Rv
Y2EuY29tL1NlY3VyZUNlcnRpZmljYXRlU2VydmljZXMuY3JsMDmgN6A1hjNodHRw
Oi8vY3JsLmNvbW9kby5uZXQvU2VjdXJlQ2VydGlmaWNhdGVTZXJ2aWNlcy5jcmww
DQYJKoZIhvcNAQEFBQADggEBAIcBbSMdflsXfcFhMs+P5/OKlFlm4J4oqF7Tt/Q0
5qo5spcWxYJvMqTpjOev/e/C6LlLqqP05tqNZSH7uoDrJiiFGv45jN5bBAS0VPmj
Z55B+glSzAVIqMk/IQQezkhr/IXownuvf7fM+F86/TXGDe+X3EyrEeFryzHRbPtI
gKvcnDe4IRRLDXE97IMzbtFuMhbsmMcWi1mmNKsFVy2T96oTy9IT4rcuO81rUBcJ
aD61JlfutuC23bkpgHl9j6PwpCikFcSF9CfUa7/lXORlAnZUtOM3ZiTTGWHIUhDl
izeauan5Hb/qmZJhlv8BzaFfDbxxvA6sCx1HRR3B7Hzs/Sk=
-----END CERTIFICATE-----

# Issuer: CN=Trusted Certificate Services O=Comodo CA Limited
# Subject: CN=Trusted Certificate Services O=Comodo CA Limited
# Label: "Comodo Trusted Services root"
# Serial: 1
# MD5 Fingerprint: 91:1b:3f:6e:cd:9e:ab:ee:07:fe:1f:71:d2:b3:61:27
# SHA1 Fingerprint: e1:9f:e3:0e:8b:84:60:9e:80:9b:17:0d:72:a8:c5:ba:6e:14:09:bd
# SHA256 Fingerprint: 3f:06:e5:56:81:d4:96:f5:be:16:9e:b5:38:9f:9f:2b:8f:f6:1e:17:08:df:68:81:72:48:49:cd:5d:27:cb:69
-----BEGIN CERTIFICATE-----
MIIEQzCCAyugAwIBAgIBATANBgkqhkiG9w0BAQUFADB/MQswCQYDVQQGEwJHQjEb
MBkGA1UECAwSR3JlYXRlciBNYW5jaGVzdGVyMRAwDgYDVQQHDAdTYWxmb3JkMRow
GAYDVQQKDBFDb21vZG8gQ0EgTGltaXRlZDElMCMGA1UEAwwcVHJ1c3RlZCBDZXJ0
aWZpY2F0ZSBTZXJ2aWNlczAeFw0wNDAxMDEwMDAwMDBaFw0yODEyMzEyMzU5NTla
MH8xCzAJBgNVBAYTAkdCMRswGQYDVQQIDBJHcmVhdGVyIE1hbmNoZXN0ZXIxEDAO
BgNVBAcMB1NhbGZvcmQxGjAYBgNVBAoMEUNvbW9kbyBDQSBMaW1pdGVkMSUwIwYD
VQQDDBxUcnVzdGVkIENlcnRpZmljYXRlIFNlcnZpY2VzMIIBIjANBgkqhkiG9w0B
AQEFAAOCAQ8AMIIBCgKCAQEA33FvNlhTWvI2VFeAxHQIIO0Yfyod5jWaHiWsnOWW
fnJSoBVC21ndZHoa0Lh73TkVvFVIxO06AOoxEbrycXQaZ7jPM8yoMa+j49d/vzMt
TGo87IvDktJTdyR0nAducPy9C1t2ul/y/9c3S0pgePfw+spwtOpZqqPOSC+pw7IL
fhdyFgymBwwbOM/JYrc/oJOlh0Hyt3BAd9i+FHzjqMB6juljatEPmsbS9Is6FARW
1O24zG71++IsWL1/T2sr92AkWCTOJu80kTrV44HQsvAEAtdbtz6SrGsSivnkBbA7
kUlcsutT6vifR4buv5XAwAaf0lteERv0xwQ1KdJVXOTt6wIDAQABo4HJMIHGMB0G
A1UdDgQWBBTFe1i97doladL3WRaoszLAeydb9DAOBgNVHQ8BAf8EBAMCAQYwDwYD
VR0TAQH/BAUwAwEB/zCBgwYDVR0fBHwwejA8oDqgOIY2aHR0cDovL2NybC5jb21v
ZG9jYS5jb20vVHJ1c3RlZENlcnRpZmljYXRlU2VydmljZXMuY3JsMDqgOKA2hjRo
dHRwOi8vY3JsLmNvbW9kby5uZXQvVHJ1c3RlZENlcnRpZmljYXRlU2VydmljZXMu
Y3JsMA0GCSqGSIb3DQEBBQUAA4IBAQDIk4E7ibSvuIQSTI3S8NtwuleGFTQQuS9/
HrCoiWChisJ3DFBKmwCL2Iv0QeLQg4pKHBQGsKNoBXAxMKdTmw7pSqBYaWcOrp32
pSxBvzwGa+RZzG0Q8ZZvH9/0BAKkn0U+yNj6NkZEUD+Cl5EfKNsYEYwq5GWDVxIS
jBc/lDb+XbDABHcTuPQV1T84zJQ6VdCsmPW6AF/ghhmBeC8owH7TzEIK9a5QoNE+
xqFx7D+gIIxmOom0jtTYsU0lR+4viMi14QVFwL4Ucd56/Y57fU0IlqUSc/Atyjcn
dBInTMu2l+nZrghtWjlA3QVHdWpaIbOjGM9O9y5Xt5hwXsjEeLBi
-----END CERTIFICATE-----

# Issuer: CN=QuoVadis Root Certification Authority O=QuoVadis Limited OU=Root Certification Authority
# Subject: CN=QuoVadis Root Certification Authority O=QuoVadis Limited OU=Root Certification Authority
# Label: "QuoVadis Root CA"
# Serial: 985026699
# MD5 Fingerprint: 27:de:36:fe:72:b7:00:03:00:9d:f4:f0:1e:6c:04:24
# SHA1 Fingerprint: de:3f:40:bd:50:93:d3:9b:6c:60:f6:da:bc:07:62:01:00:89:76:c9
# SHA256 Fingerprint: a4:5e:de:3b:bb:f0:9c:8a:e1:5c:72:ef:c0:72:68:d6:93:a2:1c:99:6f:d5:1e:67:ca:07:94:60:fd:6d:88:73
-----BEGIN CERTIFICATE-----
MIIF0DCCBLigAwIBAgIEOrZQizANBgkqhkiG9w0BAQUFADB/MQswCQYDVQQGEwJC
TTEZMBcGA1UEChMQUXVvVmFkaXMgTGltaXRlZDElMCMGA1UECxMcUm9vdCBDZXJ0
aWZpY2F0aW9uIEF1dGhvcml0eTEuMCwGA1UEAxMlUXVvVmFkaXMgUm9vdCBDZXJ0
aWZpY2F0aW9uIEF1dGhvcml0eTAeFw0wMTAzMTkxODMzMzNaFw0yMTAzMTcxODMz
MzNaMH8xCzAJBgNVBAYTAkJNMRkwFwYDVQQKExBRdW9WYWRpcyBMaW1pdGVkMSUw
IwYDVQQLExxSb290IENlcnRpZmljYXRpb24gQXV0aG9yaXR5MS4wLAYDVQQDEyVR
dW9WYWRpcyBSb290IENlcnRpZmljYXRpb24gQXV0aG9yaXR5MIIBIjANBgkqhkiG
9w0BAQEFAAOCAQ8AMIIBCgKCAQEAv2G1lVO6V/z68mcLOhrfEYBklbTRvM16z/Yp
li4kVEAkOPcahdxYTMukJ0KX0J+DisPkBgNbAKVRHnAEdOLB1Dqr1607BxgFjv2D
rOpm2RgbaIr1VxqYuvXtdj182d6UajtLF8HVj71lODqV0D1VNk7feVcxKh7YWWVJ
WCCYfqtffp/p1k3sg3Spx2zY7ilKhSoGFPlU5tPaZQeLYzcS19Dsw3sgQUSj7cug
F+FxZc4dZjH3dgEZyH0DWLaVSR2mEiboxgx24ONmy+pdpibu5cxfvWenAScOospU
xbF6lR1xHkopigPcakXBpBlebzbNw6Kwt/5cOOJSvPhEQ+aQuwIDAQABo4ICUjCC
Ak4wPQYIKwYBBQUHAQEEMTAvMC0GCCsGAQUFBzABhiFodHRwczovL29jc3AucXVv
dmFkaXNvZmZzaG9yZS5jb20wDwYDVR0TAQH/BAUwAwEB/zCCARoGA1UdIASCAREw
ggENMIIBCQYJKwYBBAG+WAABMIH7MIHUBggrBgEFBQcCAjCBxxqBxFJlbGlhbmNl
IG9uIHRoZSBRdW9WYWRpcyBSb290IENlcnRpZmljYXRlIGJ5IGFueSBwYXJ0eSBh
c3N1bWVzIGFjY2VwdGFuY2Ugb2YgdGhlIHRoZW4gYXBwbGljYWJsZSBzdGFuZGFy
ZCB0ZXJtcyBhbmQgY29uZGl0aW9ucyBvZiB1c2UsIGNlcnRpZmljYXRpb24gcHJh
Y3RpY2VzLCBhbmQgdGhlIFF1b1ZhZGlzIENlcnRpZmljYXRlIFBvbGljeS4wIgYI
KwYBBQUHAgEWFmh0dHA6Ly93d3cucXVvdmFkaXMuYm0wHQYDVR0OBBYEFItLbe3T
KbkGGew5Oanwl4Rqy+/fMIGuBgNVHSMEgaYwgaOAFItLbe3TKbkGGew5Oanwl4Rq
y+/foYGEpIGBMH8xCzAJBgNVBAYTAkJNMRkwFwYDVQQKExBRdW9WYWRpcyBMaW1p
dGVkMSUwIwYDVQQLExxSb290IENlcnRpZmljYXRpb24gQXV0aG9yaXR5MS4wLAYD
VQQDEyVRdW9WYWRpcyBSb290IENlcnRpZmljYXRpb24gQXV0aG9yaXR5ggQ6tlCL
MA4GA1UdDwEB/wQEAwIBBjANBgkqhkiG9w0BAQUFAAOCAQEAitQUtf70mpKnGdSk
fnIYj9lofFIk3WdvOXrEql494liwTXCYhGHoG+NpGA7O+0dQoE7/8CQfvbLO9Sf8
7C9TqnN7Az10buYWnuulLsS/VidQK2K6vkscPFVcQR0kvoIgR13VRH56FmjffU1R
cHhXHTMe/QKZnAzNCgVPx7uOpHX6Sm2xgI4JVrmcGmD+XcHXetwReNDWXcG31a0y
mQM6isxUJTkxgXsTIlG6Rmyhu576BGxJJnSP0nPrzDCi5upZIof4l/UO/erMkqQW
xFIY6iHOsfHmhIHluqmGKPJDWl0Snawe2ajlCmqnf6CHKc/yiU3U7MXi5nrQNiOK
SnQ2+Q==
-----END CERTIFICATE-----

# Issuer: CN=QuoVadis Root CA 2 O=QuoVadis Limited
# Subject: CN=QuoVadis Root CA 2 O=QuoVadis Limited
# Label: "QuoVadis Root CA 2"
# Serial: 1289
# MD5 Fingerprint: 5e:39:7b:dd:f8:ba:ec:82:e9:ac:62:ba:0c:54:00:2b
# SHA1 Fingerprint: ca:3a:fb:cf:12:40:36:4b:44:b2:16:20:88:80:48:39:19:93:7c:f7
# SHA256 Fingerprint: 85:a0:dd:7d:d7:20:ad:b7:ff:05:f8:3d:54:2b:20:9d:c7:ff:45:28:f7:d6:77:b1:83:89:fe:a5:e5:c4:9e:86
-----BEGIN CERTIFICATE-----
MIIFtzCCA5+gAwIBAgICBQkwDQYJKoZIhvcNAQEFBQAwRTELMAkGA1UEBhMCQk0x
GTAXBgNVBAoTEFF1b1ZhZGlzIExpbWl0ZWQxGzAZBgNVBAMTElF1b1ZhZGlzIFJv
b3QgQ0EgMjAeFw0wNjExMjQxODI3MDBaFw0zMTExMjQxODIzMzNaMEUxCzAJBgNV
BAYTAkJNMRkwFwYDVQQKExBRdW9WYWRpcyBMaW1pdGVkMRswGQYDVQQDExJRdW9W
YWRpcyBSb290IENBIDIwggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQCa
GMpLlA0ALa8DKYrwD4HIrkwZhR0In6spRIXzL4GtMh6QRr+jhiYaHv5+HBg6XJxg
Fyo6dIMzMH1hVBHL7avg5tKifvVrbxi3Cgst/ek+7wrGsxDp3MJGF/hd/aTa/55J
WpzmM+Yklvc/ulsrHHo1wtZn/qtmUIttKGAr79dgw8eTvI02kfN/+NsRE8Scd3bB
rrcCaoF6qUWD4gXmuVbBlDePSHFjIuwXZQeVikvfj8ZaCuWw419eaxGrDPmF60Tp
+ARz8un+XJiM9XOva7R+zdRcAitMOeGylZUtQofX1bOQQ7dsE/He3fbE+Ik/0XX1
ksOR1YqI0JDs3G3eicJlcZaLDQP9nL9bFqyS2+r+eXyt66/3FsvbzSUr5R/7mp/i
Ucw6UwxI5g69ybR2BlLmEROFcmMDBOAENisgGQLodKcftslWZvB1JdxnwQ5hYIiz
PtGo/KPaHbDRsSNU30R2be1B2MGyIrZTHN81Hdyhdyox5C315eXbyOD/5YDXC2Og
/zOhD7osFRXql7PSorW+8oyWHhqPHWykYTe5hnMz15eWniN9gqRMgeKh0bpnX5UH
oycR7hYQe7xFSkyyBNKr79X9DFHOUGoIMfmR2gyPZFwDwzqLID9ujWc9Otb+fVuI
yV77zGHcizN300QyNQliBJIWENieJ0f7OyHj+OsdWwIDAQABo4GwMIGtMA8GA1Ud
EwEB/wQFMAMBAf8wCwYDVR0PBAQDAgEGMB0GA1UdDgQWBBQahGK8SEwzJQTU7tD2
A8QZRtGUazBuBgNVHSMEZzBlgBQahGK8SEwzJQTU7tD2A8QZRtGUa6FJpEcwRTEL
MAkGA1UEBhMCQk0xGTAXBgNVBAoTEFF1b1ZhZGlzIExpbWl0ZWQxGzAZBgNVBAMT
ElF1b1ZhZGlzIFJvb3QgQ0EgMoICBQkwDQYJKoZIhvcNAQEFBQADggIBAD4KFk2f
BluornFdLwUvZ+YTRYPENvbzwCYMDbVHZF34tHLJRqUDGCdViXh9duqWNIAXINzn
g/iN/Ae42l9NLmeyhP3ZRPx3UIHmfLTJDQtyU/h2BwdBR5YM++CCJpNVjP4iH2Bl
fF/nJrP3MpCYUNQ3cVX2kiF495V5+vgtJodmVjB3pjd4M1IQWK4/YY7yarHvGH5K
WWPKjaJW1acvvFYfzznB4vsKqBUsfU16Y8Zsl0Q80m/DShcK+JDSV6IZUaUtl0Ha
B0+pUNqQjZRG4T7wlP0QADj1O+hA4bRuVhogzG9Yje0uRY/W6ZM/57Es3zrWIozc
hLsib9D45MY56QSIPMO661V6bYCZJPVsAfv4l7CUW+v90m/xd2gNNWQjrLhVoQPR
TUIZ3Ph1WVaj+ahJefivDrkRoHy3au000LYmYjgahwz46P0u05B/B5EqHdZ+XIWD
mbA4CD/pXvk1B+TJYm5Xf6dQlfe6yJvmjqIBxdZmv3lh8zwc4bmCXF2gw+nYSL0Z
ohEUGW6yhhtoPkg3Goi3XZZenMfvJ2II4pEZXNLxId26F0KCl3GBUzGpn/Z9Yr9y
4aOTHcyKJloJONDO1w2AFrR4pTqHTI2KpdVGl/IsELm8VCLAAVBpQ570su9t+Oza
8eOx79+Rj1QqCyXBJhnEUhAFZdWCEOrCMc0u
-----END CERTIFICATE-----

# Issuer: CN=QuoVadis Root CA 3 O=QuoVadis Limited
# Subject: CN=QuoVadis Root CA 3 O=QuoVadis Limited
# Label: "QuoVadis Root CA 3"
# Serial: 1478
# MD5 Fingerprint: 31:85:3c:62:94:97:63:b9:aa:fd:89:4e:af:6f:e0:cf
# SHA1 Fingerprint: 1f:49:14:f7:d8:74:95:1d:dd:ae:02:c0:be:fd:3a:2d:82:75:51:85
# SHA256 Fingerprint: 18:f1:fc:7f:20:5d:f8:ad:dd:eb:7f:e0:07:dd:57:e3:af:37:5a:9c:4d:8d:73:54:6b:f4:f1:fe:d1:e1:8d:35
-----BEGIN CERTIFICATE-----
MIIGnTCCBIWgAwIBAgICBcYwDQYJKoZIhvcNAQEFBQAwRTELMAkGA1UEBhMCQk0x
GTAXBgNVBAoTEFF1b1ZhZGlzIExpbWl0ZWQxGzAZBgNVBAMTElF1b1ZhZGlzIFJv
b3QgQ0EgMzAeFw0wNjExMjQxOTExMjNaFw0zMTExMjQxOTA2NDRaMEUxCzAJBgNV
BAYTAkJNMRkwFwYDVQQKExBRdW9WYWRpcyBMaW1pdGVkMRswGQYDVQQDExJRdW9W
YWRpcyBSb290IENBIDMwggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQDM
V0IWVJzmmNPTTe7+7cefQzlKZbPoFog02w1ZkXTPkrgEQK0CSzGrvI2RaNggDhoB
4hp7Thdd4oq3P5kazethq8Jlph+3t723j/z9cI8LoGe+AaJZz3HmDyl2/7FWeUUr
H556VOijKTVopAFPD6QuN+8bv+OPEKhyq1hX51SGyMnzW9os2l2ObjyjPtr7guXd
8lyyBTNvijbO0BNO/79KDDRMpsMhvVAEVeuxu537RR5kFd5VAYwCdrXLoT9Cabwv
vWhDFlaJKjdhkf2mrk7AyxRllDdLkgbvBNDInIjbC3uBr7E9KsRlOni27tyAsdLT
mZw67mtaa7ONt9XOnMK+pUsvFrGeaDsGb659n/je7Mwpp5ijJUMv7/FfJuGITfhe
btfZFG4ZM2mnO4SJk8RTVROhUXhA+LjJou57ulJCg54U7QVSWllWp5f8nT8KKdjc
T5EOE7zelaTfi5m+rJsziO+1ga8bxiJTyPbH7pcUsMV8eFLI8M5ud2CEpukqdiDt
WAEXMJPpGovgc2PZapKUSU60rUqFxKMiMPwJ7Wgic6aIDFUhWMXhOp8q3crhkODZ
c6tsgLjoC2SToJyMGf+z0gzskSaHirOi4XCPLArlzW1oUevaPwV/izLmE1xr/l9A
4iLItLRkT9a6fUg+qGkM17uGcclzuD87nSVL2v9A6wIDAQABo4IBlTCCAZEwDwYD
VR0TAQH/BAUwAwEB/zCB4QYDVR0gBIHZMIHWMIHTBgkrBgEEAb5YAAMwgcUwgZMG
CCsGAQUFBwICMIGGGoGDQW55IHVzZSBvZiB0aGlzIENlcnRpZmljYXRlIGNvbnN0
aXR1dGVzIGFjY2VwdGFuY2Ugb2YgdGhlIFF1b1ZhZGlzIFJvb3QgQ0EgMyBDZXJ0
aWZpY2F0ZSBQb2xpY3kgLyBDZXJ0aWZpY2F0aW9uIFByYWN0aWNlIFN0YXRlbWVu
dC4wLQYIKwYBBQUHAgEWIWh0dHA6Ly93d3cucXVvdmFkaXNnbG9iYWwuY29tL2Nw
czALBgNVHQ8EBAMCAQYwHQYDVR0OBBYEFPLAE+CCQz777i9nMpY1XNu4ywLQMG4G
A1UdIwRnMGWAFPLAE+CCQz777i9nMpY1XNu4ywLQoUmkRzBFMQswCQYDVQQGEwJC
TTEZMBcGA1UEChMQUXVvVmFkaXMgTGltaXRlZDEbMBkGA1UEAxMSUXVvVmFkaXMg
Um9vdCBDQSAzggIFxjANBgkqhkiG9w0BAQUFAAOCAgEAT62gLEz6wPJv92ZVqyM0
7ucp2sNbtrCD2dDQ4iH782CnO11gUyeim/YIIirnv6By5ZwkajGxkHon24QRiSem
d1o417+shvzuXYO8BsbRd2sPbSQvS3pspweWyuOEn62Iix2rFo1bZhfZFvSLgNLd
+LJ2w/w4E6oM3kJpK27zPOuAJ9v1pkQNn1pVWQvVDVJIxa6f8i+AxeoyUDUSly7B
4f/xI4hROJ/yZlZ25w9Rl6VSDE1JUZU2Pb+iSwwQHYaZTKrzchGT5Or2m9qoXadN
t54CrnMAyNojA+j56hl0YgCUyyIgvpSnWbWCar6ZeXqp8kokUvd0/bpO5qgdAm6x
DYBEwa7TIzdfu4V8K5Iu6H6li92Z4b8nby1dqnuH/grdS/yO9SbkbnBCbjPsMZ57
k8HkyWkaPcBrTiJt7qtYTcbQQcEr6k8Sh17rRdhs9ZgC06DYVYoGmRmioHfRMJ6s
zHXug/WwYjnPbFfiTNKRCw51KBuav/0aQ/HKd/s7j2G4aSgWQgRecCocIdiP4b0j
Wy10QJLZYxkNc91pvGJHvOB0K7Lrfb5BG7XARsWhIstfTsEokt4YutUqKLsRixeT
mJlglFwjz1onl14LBQaTNx47aTbrqZ5hHY8y2o4M1nQ+ewkk2gF3R8Q7zTSMmfXK
4SVhM7JZG+Ju1zdXtg2pEto=
-----END CERTIFICATE-----

# Issuer: O=SECOM Trust.net OU=Security Communication RootCA1
# Subject: O=SECOM Trust.net OU=Security Communication RootCA1
# Label: "Security Communication Root CA"
# Serial: 0
# MD5 Fingerprint: f1:bc:63:6a:54:e0:b5:27:f5:cd:e7:1a:e3:4d:6e:4a
# SHA1 Fingerprint: 36:b1:2b:49:f9:81:9e:d7:4c:9e:bc:38:0f:c6:56:8f:5d:ac:b2:f7
# SHA256 Fingerprint: e7:5e:72:ed:9f:56:0e:ec:6e:b4:80:00:73:a4:3f:c3:ad:19:19:5a:39:22:82:01:78:95:97:4a:99:02:6b:6c
-----BEGIN CERTIFICATE-----
MIIDWjCCAkKgAwIBAgIBADANBgkqhkiG9w0BAQUFADBQMQswCQYDVQQGEwJKUDEY
MBYGA1UEChMPU0VDT00gVHJ1c3QubmV0MScwJQYDVQQLEx5TZWN1cml0eSBDb21t
dW5pY2F0aW9uIFJvb3RDQTEwHhcNMDMwOTMwMDQyMDQ5WhcNMjMwOTMwMDQyMDQ5
WjBQMQswCQYDVQQGEwJKUDEYMBYGA1UEChMPU0VDT00gVHJ1c3QubmV0MScwJQYD
VQQLEx5TZWN1cml0eSBDb21tdW5pY2F0aW9uIFJvb3RDQTEwggEiMA0GCSqGSIb3
DQEBAQUAA4IBDwAwggEKAoIBAQCzs/5/022x7xZ8V6UMbXaKL0u/ZPtM7orw8yl8
9f/uKuDp6bpbZCKamm8sOiZpUQWZJtzVHGpxxpp9Hp3dfGzGjGdnSj74cbAZJ6kJ
DKaVv0uMDPpVmDvY6CKhS3E4eayXkmmziX7qIWgGmBSWh9JhNrxtJ1aeV+7AwFb9
Ms+k2Y7CI9eNqPPYJayX5HA49LY6tJ07lyZDo6G8SVlyTCMwhwFY9k6+HGhWZq/N
QV3Is00qVUarH9oe4kA92819uZKAnDfdDJZkndwi92SL32HeFZRSFaB9UslLqCHJ
xrHty8OVYNEP8Ktw+N/LTX7s1vqr2b1/VPKl6Xn62dZ2JChzAgMBAAGjPzA9MB0G
A1UdDgQWBBSgc0mZaNyFW2XjmygvV5+9M7wHSDALBgNVHQ8EBAMCAQYwDwYDVR0T
AQH/BAUwAwEB/zANBgkqhkiG9w0BAQUFAAOCAQEAaECpqLvkT115swW1F7NgE+vG
kl3g0dNq/vu+m22/xwVtWSDEHPC32oRYAmP6SBbvT6UL90qY8j+eG61Ha2POCEfr
Uj94nK9NrvjVT8+amCoQQTlSxN3Zmw7vkwGusi7KaEIkQmywszo+zenaSMQVy+n5
Bw+SUEmK3TGXX8npN6o7WWWXlDLJs58+OmJYxUmtYg5xpTKqL8aJdkNAExNnPaJU
JRDL8Try2frbSVa7pv6nQTXD4IhhyYjH3zYQIphZ6rBK+1YWc26sTfcioU+tHXot
RSflMMFe8toTyyVCUZVHA4xsIcx0Qu1T/zOLjw9XARYvz6buyXAiFL39vmwLAw==
-----END CERTIFICATE-----

# Issuer: CN=Sonera Class2 CA O=Sonera
# Subject: CN=Sonera Class2 CA O=Sonera
# Label: "Sonera Class 2 Root CA"
# Serial: 29
# MD5 Fingerprint: a3:ec:75:0f:2e:88:df:fa:48:01:4e:0b:5c:48:6f:fb
# SHA1 Fingerprint: 37:f7:6d:e6:07:7c:90:c5:b1:3e:93:1a:b7:41:10:b4:f2:e4:9a:27
# SHA256 Fingerprint: 79:08:b4:03:14:c1:38:10:0b:51:8d:07:35:80:7f:fb:fc:f8:51:8a:00:95:33:71:05:ba:38:6b:15:3d:d9:27
-----BEGIN CERTIFICATE-----
MIIDIDCCAgigAwIBAgIBHTANBgkqhkiG9w0BAQUFADA5MQswCQYDVQQGEwJGSTEP
MA0GA1UEChMGU29uZXJhMRkwFwYDVQQDExBTb25lcmEgQ2xhc3MyIENBMB4XDTAx
MDQwNjA3Mjk0MFoXDTIxMDQwNjA3Mjk0MFowOTELMAkGA1UEBhMCRkkxDzANBgNV
BAoTBlNvbmVyYTEZMBcGA1UEAxMQU29uZXJhIENsYXNzMiBDQTCCASIwDQYJKoZI
hvcNAQEBBQADggEPADCCAQoCggEBAJAXSjWdyvANlsdE+hY3/Ei9vX+ALTU74W+o
Z6m/AxxNjG8yR9VBaKQTBME1DJqEQ/xcHf+Js+gXGM2RX/uJ4+q/Tl18GybTdXnt
5oTjV+WtKcT0OijnpXuENmmz/V52vaMtmdOQTiMofRhj8VQ7Jp12W5dCsv+u8E7s
3TmVToMGf+dJQMjFAbJUWmYdPfz56TwKnoG4cPABi+QjVHzIrviQHgCWctRUz2Ej
vOr7nQKV0ba5cTppCD8PtOFCx4j1P5iop7oc4HFx71hXgVB6XGt0Rg6DA5jDjqhu
8nYybieDwnPz3BjotJPqdURrBGAgcVeHnfO+oJAjPYok4doh28MCAwEAAaMzMDEw
DwYDVR0TAQH/BAUwAwEB/zARBgNVHQ4ECgQISqCqWITTXjwwCwYDVR0PBAQDAgEG
MA0GCSqGSIb3DQEBBQUAA4IBAQBazof5FnIVV0sd2ZvnoiYw7JNn39Yt0jSv9zil
zqsWuasvfDXLrNAPtEwr/IDva4yRXzZ299uzGxnq9LIR/WFxRL8oszodv7ND6J+/
3DEIcbCdjdY0RzKQxmUk96BKfARzjzlvF4xytb1LyHr4e4PDKE6cCepnP7JnBBvD
FNr450kkkdAdavphOe9r5yF1BgfYErQhIHBCcYHaPJo2vqZbDWpsmh+Re/n570K6
Tk6ezAyNlNzZRZxe7EJQY670XcSxEtzKO6gunRRaBXW37Ndj4ro1tgQIkejanZz2
ZrUYrAqmVCY0M9IbwdR/GjqOC6oybtv8TyWf2TLHllpwrN9M
-----END CERTIFICATE-----

# Issuer: CN=Staat der Nederlanden Root CA O=Staat der Nederlanden
# Subject: CN=Staat der Nederlanden Root CA O=Staat der Nederlanden
# Label: "Staat der Nederlanden Root CA"
# Serial: 10000010
# MD5 Fingerprint: 60:84:7c:5a:ce:db:0c:d4:cb:a7:e9:fe:02:c6:a9:c0
# SHA1 Fingerprint: 10:1d:fa:3f:d5:0b:cb:bb:9b:b5:60:0c:19:55:a4:1a:f4:73:3a:04
# SHA256 Fingerprint: d4:1d:82:9e:8c:16:59:82:2a:f9:3f:ce:62:bf:fc:de:26:4f:c8:4e:8b:95:0c:5f:f2:75:d0:52:35:46:95:a3
-----BEGIN CERTIFICATE-----
MIIDujCCAqKgAwIBAgIEAJiWijANBgkqhkiG9w0BAQUFADBVMQswCQYDVQQGEwJO
TDEeMBwGA1UEChMVU3RhYXQgZGVyIE5lZGVybGFuZGVuMSYwJAYDVQQDEx1TdGFh
dCBkZXIgTmVkZXJsYW5kZW4gUm9vdCBDQTAeFw0wMjEyMTcwOTIzNDlaFw0xNTEy
MTYwOTE1MzhaMFUxCzAJBgNVBAYTAk5MMR4wHAYDVQQKExVTdGFhdCBkZXIgTmVk
ZXJsYW5kZW4xJjAkBgNVBAMTHVN0YWF0IGRlciBOZWRlcmxhbmRlbiBSb290IENB
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAmNK1URF6gaYUmHFtvszn
ExvWJw56s2oYHLZhWtVhCb/ekBPHZ+7d89rFDBKeNVU+LCeIQGv33N0iYfXCxw71
9tV2U02PjLwYdjeFnejKScfST5gTCaI+Ioicf9byEGW07l8Y1Rfj+MX94p2i71MO
hXeiD+EwR+4A5zN9RGcaC1Hoi6CeUJhoNFIfLm0B8mBF8jHrqTFoKbt6QZ7GGX+U
tFE5A3+y3qcym7RHjm+0Sq7lr7HcsBthvJly3uSJt3omXdozSVtSnA71iq3DuD3o
BmrC1SoLbHuEvVYFy4ZlkuxEK7COudxwC0barbxjiDn622r+I/q85Ej0ZytqERAh
SQIDAQABo4GRMIGOMAwGA1UdEwQFMAMBAf8wTwYDVR0gBEgwRjBEBgRVHSAAMDww
OgYIKwYBBQUHAgEWLmh0dHA6Ly93d3cucGtpb3ZlcmhlaWQubmwvcG9saWNpZXMv
cm9vdC1wb2xpY3kwDgYDVR0PAQH/BAQDAgEGMB0GA1UdDgQWBBSofeu8Y6R0E3QA
7Jbg0zTBLL9s+DANBgkqhkiG9w0BAQUFAAOCAQEABYSHVXQ2YcG70dTGFagTtJ+k
/rvuFbQvBgwp8qiSpGEN/KtcCFtREytNwiphyPgJWPwtArI5fZlmgb9uXJVFIGzm
eafR2Bwp/MIgJ1HI8XxdNGdphREwxgDS1/PTfLbwMVcoEoJz6TMvplW0C5GUR5z6
u3pCMuiufi3IvKwUv9kP2Vv8wfl6leF9fpb8cbDCTMjfRTTJzg3ynGQI0DvDKcWy
7ZAEwbEpkcUwb8GpcjPM/l0WFywRaed+/sWDCN+83CI6LiBpIzlWYGeQiy52OfsR
iJf2fL1LuCAWZwWN4jvBcj+UlTfHXbme2JOhF4//DGYVwSR8MnwDHTuhWEUykw==
-----END CERTIFICATE-----

# Issuer: CN=UTN - DATACorp SGC O=The USERTRUST Network OU=http://www.usertrust.com
# Subject: CN=UTN - DATACorp SGC O=The USERTRUST Network OU=http://www.usertrust.com
# Label: "UTN DATACorp SGC Root CA"
# Serial: 91374294542884689855167577680241077609
# MD5 Fingerprint: b3:a5:3e:77:21:6d:ac:4a:c0:c9:fb:d5:41:3d:ca:06
# SHA1 Fingerprint: 58:11:9f:0e:12:82:87:ea:50:fd:d9:87:45:6f:4f:78:dc:fa:d6:d4
# SHA256 Fingerprint: 85:fb:2f:91:dd:12:27:5a:01:45:b6:36:53:4f:84:02:4a:d6:8b:69:b8:ee:88:68:4f:f7:11:37:58:05:b3:48
-----BEGIN CERTIFICATE-----
MIIEXjCCA0agAwIBAgIQRL4Mi1AAIbQR0ypoBqmtaTANBgkqhkiG9w0BAQUFADCB
kzELMAkGA1UEBhMCVVMxCzAJBgNVBAgTAlVUMRcwFQYDVQQHEw5TYWx0IExha2Ug
Q2l0eTEeMBwGA1UEChMVVGhlIFVTRVJUUlVTVCBOZXR3b3JrMSEwHwYDVQQLExho
dHRwOi8vd3d3LnVzZXJ0cnVzdC5jb20xGzAZBgNVBAMTElVUTiAtIERBVEFDb3Jw
IFNHQzAeFw05OTA2MjQxODU3MjFaFw0xOTA2MjQxOTA2MzBaMIGTMQswCQYDVQQG
EwJVUzELMAkGA1UECBMCVVQxFzAVBgNVBAcTDlNhbHQgTGFrZSBDaXR5MR4wHAYD
VQQKExVUaGUgVVNFUlRSVVNUIE5ldHdvcmsxITAfBgNVBAsTGGh0dHA6Ly93d3cu
dXNlcnRydXN0LmNvbTEbMBkGA1UEAxMSVVROIC0gREFUQUNvcnAgU0dDMIIBIjAN
BgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA3+5YEKIrblXEjr8uRgnn4AgPLit6
E5Qbvfa2gI5lBZMAHryv4g+OGQ0SR+ysraP6LnD43m77VkIVni5c7yPeIbkFdicZ
D0/Ww5y0vpQZY/KmEQrrU0icvvIpOxboGqBMpsn0GFlowHDyUwDAXlCCpVZvNvlK
4ESGoE1O1kduSUrLZ9emxAW5jh70/P/N5zbgnAVssjMiFdC04MwXwLLA9P4yPykq
lXvY8qdOD1R8oQ2AswkDwf9c3V6aPryuvEeKaq5xyh+xKrhfQgUL7EYw0XILyulW
bfXv33i+Ybqypa4ETLyorGkVl73v67SMvzX41MPRKA5cOp9wGDMgd8SirwIDAQAB
o4GrMIGoMAsGA1UdDwQEAwIBxjAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRT
MtGzz3/64PGgXYVOktKeRR20TzA9BgNVHR8ENjA0MDKgMKAuhixodHRwOi8vY3Js
LnVzZXJ0cnVzdC5jb20vVVROLURBVEFDb3JwU0dDLmNybDAqBgNVHSUEIzAhBggr
BgEFBQcDAQYKKwYBBAGCNwoDAwYJYIZIAYb4QgQBMA0GCSqGSIb3DQEBBQUAA4IB
AQAnNZcAiosovcYzMB4p/OL31ZjUQLtgyr+rFywJNn9Q+kHcrpY6CiM+iVnJowft
Gzet/Hy+UUla3joKVAgWRcKZsYfNjGjgaQPpxE6YsjuMFrMOoAyYUJuTqXAJyCyj
j98C5OBxOvG0I3KgqgHf35g+FFCgMSa9KOlaMCZ1+XtgHI3zzVAmbQQnmt/VDUVH
KWss5nbZqSl9Mt3JNjy9rjXxEZ4du5A/EkdOjtd+D2JzHVImOBwYSf0wdJrE5SIv
2MCN7ZF6TACPcn9d2t0bi0Vr591pl6jFVkwPDPafepE39peC4N1xaf92P2BNPM/3
mfnGV/TJVTl4uix5yaaIK/QI
-----END CERTIFICATE-----

# Issuer: CN=UTN-USERFirst-Hardware O=The USERTRUST Network OU=http://www.usertrust.com
# Subject: CN=UTN-USERFirst-Hardware O=The USERTRUST Network OU=http://www.usertrust.com
# Label: "UTN USERFirst Hardware Root CA"
# Serial: 91374294542884704022267039221184531197
# MD5 Fingerprint: 4c:56:41:e5:0d:bb:2b:e8:ca:a3:ed:18:08:ad:43:39
# SHA1 Fingerprint: 04:83:ed:33:99:ac:36:08:05:87:22:ed:bc:5e:46:00:e3:be:f9:d7
# SHA256 Fingerprint: 6e:a5:47:41:d0:04:66:7e:ed:1b:48:16:63:4a:a3:a7:9e:6e:4b:96:95:0f:82:79:da:fc:8d:9b:d8:81:21:37
-----BEGIN CERTIFICATE-----
MIIEdDCCA1ygAwIBAgIQRL4Mi1AAJLQR0zYq/mUK/TANBgkqhkiG9w0BAQUFADCB
lzELMAkGA1UEBhMCVVMxCzAJBgNVBAgTAlVUMRcwFQYDVQQHEw5TYWx0IExha2Ug
Q2l0eTEeMBwGA1UEChMVVGhlIFVTRVJUUlVTVCBOZXR3b3JrMSEwHwYDVQQLExho
dHRwOi8vd3d3LnVzZXJ0cnVzdC5jb20xHzAdBgNVBAMTFlVUTi1VU0VSRmlyc3Qt
SGFyZHdhcmUwHhcNOTkwNzA5MTgxMDQyWhcNMTkwNzA5MTgxOTIyWjCBlzELMAkG
A1UEBhMCVVMxCzAJBgNVBAgTAlVUMRcwFQYDVQQHEw5TYWx0IExha2UgQ2l0eTEe
MBwGA1UEChMVVGhlIFVTRVJUUlVTVCBOZXR3b3JrMSEwHwYDVQQLExhodHRwOi8v
d3d3LnVzZXJ0cnVzdC5jb20xHzAdBgNVBAMTFlVUTi1VU0VSRmlyc3QtSGFyZHdh
cmUwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCx98M4P7Sof885glFn
0G2f0v9Y8+efK+wNiVSZuTiZFvfgIXlIwrthdBKWHTxqctU8EGc6Oe0rE81m65UJ
M6Rsl7HoxuzBdXmcRl6Nq9Bq/bkqVRcQVLMZ8Jr28bFdtqdt++BxF2uiiPsA3/4a
MXcMmgF6sTLjKwEHOG7DpV4jvEWbe1DByTCP2+UretNb+zNAHqDVmBe8i4fDidNd
oI6yqqr2jmmIBsX6iSHzCJ1pLgkzmykNRg+MzEk0sGlRvfkGzWitZky8PqxhvQqI
DsjfPe58BEydCl5rkdbux+0ojatNh4lz0G6k0B4WixThdkQDf2Os5M1JnMWS9Ksy
oUhbAgMBAAGjgbkwgbYwCwYDVR0PBAQDAgHGMA8GA1UdEwEB/wQFMAMBAf8wHQYD
VR0OBBYEFKFyXyYbKJhDlV0HN9WFlp1L0sNFMEQGA1UdHwQ9MDswOaA3oDWGM2h0
dHA6Ly9jcmwudXNlcnRydXN0LmNvbS9VVE4tVVNFUkZpcnN0LUhhcmR3YXJlLmNy
bDAxBgNVHSUEKjAoBggrBgEFBQcDAQYIKwYBBQUHAwUGCCsGAQUFBwMGBggrBgEF
BQcDBzANBgkqhkiG9w0BAQUFAAOCAQEARxkP3nTGmZev/K0oXnWO6y1n7k57K9cM
//bey1WiCuFMVGWTYGufEpytXoMs61quwOQt9ABjHbjAbPLPSbtNk28Gpgoiskli
CE7/yMgUsogWXecB5BKV5UU0s4tpvc+0hY91UZ59Ojg6FEgSxvunOxqNDYJAB+gE
CJChicsZUN/KHAG8HQQZexB2lzvukJDKxA4fFm517zP4029bHpbj4HR3dHuKom4t
3XbWOTCC8KucUvIqx69JXn7HaOWCgchqJ/kniCrVWFCVH/A7HFe7fRQ5YiuayZSS
KqMiDP+JJn1fIytH1xUdqWqeUQ0qUZ6B+dQ7XnASfxAynB67nfhmqA==
-----END CERTIFICATE-----

# Issuer: CN=Chambers of Commerce Root O=AC Camerfirma SA CIF A82743287 OU=http://www.chambersign.org
# Subject: CN=Chambers of Commerce Root O=AC Camerfirma SA CIF A82743287 OU=http://www.chambersign.org
# Label: "Camerfirma Chambers of Commerce Root"
# Serial: 0
# MD5 Fingerprint: b0:01:ee:14:d9:af:29:18:94:76:8e:f1:69:33:2a:84
# SHA1 Fingerprint: 6e:3a:55:a4:19:0c:19:5c:93:84:3c:c0:db:72:2e:31:30:61:f0:b1
# SHA256 Fingerprint: 0c:25:8a:12:a5:67:4a:ef:25:f2:8b:a7:dc:fa:ec:ee:a3:48:e5:41:e6:f5:cc:4e:e6:3b:71:b3:61:60:6a:c3
-----BEGIN CERTIFICATE-----
MIIEvTCCA6WgAwIBAgIBADANBgkqhkiG9w0BAQUFADB/MQswCQYDVQQGEwJFVTEn
MCUGA1UEChMeQUMgQ2FtZXJmaXJtYSBTQSBDSUYgQTgyNzQzMjg3MSMwIQYDVQQL
ExpodHRwOi8vd3d3LmNoYW1iZXJzaWduLm9yZzEiMCAGA1UEAxMZQ2hhbWJlcnMg
b2YgQ29tbWVyY2UgUm9vdDAeFw0wMzA5MzAxNjEzNDNaFw0zNzA5MzAxNjEzNDRa
MH8xCzAJBgNVBAYTAkVVMScwJQYDVQQKEx5BQyBDYW1lcmZpcm1hIFNBIENJRiBB
ODI3NDMyODcxIzAhBgNVBAsTGmh0dHA6Ly93d3cuY2hhbWJlcnNpZ24ub3JnMSIw
IAYDVQQDExlDaGFtYmVycyBvZiBDb21tZXJjZSBSb290MIIBIDANBgkqhkiG9w0B
AQEFAAOCAQ0AMIIBCAKCAQEAtzZV5aVdGDDg2olUkfzIx1L4L1DZ77F1c2VHfRtb
unXF/KGIJPov7coISjlUxFF6tdpg6jg8gbLL8bvZkSM/SAFwdakFKq0fcfPJVD0d
BmpAPrMMhe5cG3nCYsS4No41XQEMIwRHNaqbYE6gZj3LJgqcQKH0XZi/caulAGgq
7YN6D6IUtdQis4CwPAxaUWktWBiP7Zme8a7ileb2R6jWDA+wWFjbw2Y3npuRVDM3
0pQcakjJyfKl2qUMI/cjDpwyVV5xnIQFUZot/eZOKjRa3spAN2cMVCFVd9oKDMyX
roDclDZK9D7ONhMeU+SsTjoF7Nuucpw4i9A5O4kKPnf+dQIBA6OCAUQwggFAMBIG
A1UdEwEB/wQIMAYBAf8CAQwwPAYDVR0fBDUwMzAxoC+gLYYraHR0cDovL2NybC5j
aGFtYmVyc2lnbi5vcmcvY2hhbWJlcnNyb290LmNybDAdBgNVHQ4EFgQU45T1sU3p
26EpW1eLTXYGduHRooowDgYDVR0PAQH/BAQDAgEGMBEGCWCGSAGG+EIBAQQEAwIA
BzAnBgNVHREEIDAegRxjaGFtYmVyc3Jvb3RAY2hhbWJlcnNpZ24ub3JnMCcGA1Ud
EgQgMB6BHGNoYW1iZXJzcm9vdEBjaGFtYmVyc2lnbi5vcmcwWAYDVR0gBFEwTzBN
BgsrBgEEAYGHLgoDATA+MDwGCCsGAQUFBwIBFjBodHRwOi8vY3BzLmNoYW1iZXJz
aWduLm9yZy9jcHMvY2hhbWJlcnNyb290Lmh0bWwwDQYJKoZIhvcNAQEFBQADggEB
AAxBl8IahsAifJ/7kPMa0QOx7xP5IV8EnNrJpY0nbJaHkb5BkAFyk+cefV/2icZd
p0AJPaxJRUXcLo0waLIJuvvDL8y6C98/d3tGfToSJI6WjzwFCm/SlCgdbQzALogi
1djPHRPH8EjX1wWnz8dHnjs8NMiAT9QUu/wNUPf6s+xCX6ndbcj0dc97wXImsQEc
XCz9ek60AcUFV7nnPKoF2YjpB0ZBzu9Bga5Y34OirsrXdx/nADydb47kMgkdTXg0
eDQ8lJsm7U9xxhl6vSAiSFr+S30Dt+dYvsYyTnQeaN2oaFuzPu5ifdmA6Ap1erfu
tGWaIZDgqtCYvDi1czyL+Nw=
-----END CERTIFICATE-----

# Issuer: CN=Global Chambersign Root O=AC Camerfirma SA CIF A82743287 OU=http://www.chambersign.org
# Subject: CN=Global Chambersign Root O=AC Camerfirma SA CIF A82743287 OU=http://www.chambersign.org
# Label: "Camerfirma Global Chambersign Root"
# Serial: 0
# MD5 Fingerprint: c5:e6:7b:bf:06:d0:4f:43:ed:c4:7a:65:8a:fb:6b:19
# SHA1 Fingerprint: 33:9b:6b:14:50:24:9b:55:7a:01:87:72:84:d9:e0:2f:c3:d2:d8:e9
# SHA256 Fingerprint: ef:3c:b4:17:fc:8e:bf:6f:97:87:6c:9e:4e:ce:39:de:1e:a5:fe:64:91:41:d1:02:8b:7d:11:c0:b2:29:8c:ed
-----BEGIN CERTIFICATE-----
MIIExTCCA62gAwIBAgIBADANBgkqhkiG9w0BAQUFADB9MQswCQYDVQQGEwJFVTEn
MCUGA1UEChMeQUMgQ2FtZXJmaXJtYSBTQSBDSUYgQTgyNzQzMjg3MSMwIQYDVQQL
ExpodHRwOi8vd3d3LmNoYW1iZXJzaWduLm9yZzEgMB4GA1UEAxMXR2xvYmFsIENo
YW1iZXJzaWduIFJvb3QwHhcNMDMwOTMwMTYxNDE4WhcNMzcwOTMwMTYxNDE4WjB9
MQswCQYDVQQGEwJFVTEnMCUGA1UEChMeQUMgQ2FtZXJmaXJtYSBTQSBDSUYgQTgy
NzQzMjg3MSMwIQYDVQQLExpodHRwOi8vd3d3LmNoYW1iZXJzaWduLm9yZzEgMB4G
A1UEAxMXR2xvYmFsIENoYW1iZXJzaWduIFJvb3QwggEgMA0GCSqGSIb3DQEBAQUA
A4IBDQAwggEIAoIBAQCicKLQn0KuWxfH2H3PFIP8T8mhtxOviteePgQKkotgVvq0
Mi+ITaFgCPS3CU6gSS9J1tPfnZdan5QEcOw/Wdm3zGaLmFIoCQLfxS+EjXqXd7/s
QJ0lcqu1PzKY+7e3/HKE5TWH+VX6ox8Oby4o3Wmg2UIQxvi1RMLQQ3/bvOSiPGpV
eAp3qdjqGTK3L/5cPxvusZjsyq16aUXjlg9V9ubtdepl6DJWk0aJqCWKZQbua795
B9Dxt6/tLE2Su8CoX6dnfQTyFQhwrJLWfQTSM/tMtgsL+xrJxI0DqX5c8lCrEqWh
z0hQpe/SyBoT+rB/sYIcd2oPX9wLlY/vQ37mRQklAgEDo4IBUDCCAUwwEgYDVR0T
AQH/BAgwBgEB/wIBDDA/BgNVHR8EODA2MDSgMqAwhi5odHRwOi8vY3JsLmNoYW1i
ZXJzaWduLm9yZy9jaGFtYmVyc2lnbnJvb3QuY3JsMB0GA1UdDgQWBBRDnDafsJ4w
TcbOX60Qq+UDpfqpFDAOBgNVHQ8BAf8EBAMCAQYwEQYJYIZIAYb4QgEBBAQDAgAH
MCoGA1UdEQQjMCGBH2NoYW1iZXJzaWducm9vdEBjaGFtYmVyc2lnbi5vcmcwKgYD
VR0SBCMwIYEfY2hhbWJlcnNpZ25yb290QGNoYW1iZXJzaWduLm9yZzBbBgNVHSAE
VDBSMFAGCysGAQQBgYcuCgEBMEEwPwYIKwYBBQUHAgEWM2h0dHA6Ly9jcHMuY2hh
bWJlcnNpZ24ub3JnL2Nwcy9jaGFtYmVyc2lnbnJvb3QuaHRtbDANBgkqhkiG9w0B
AQUFAAOCAQEAPDtwkfkEVCeR4e3t/mh/YV3lQWVPMvEYBZRqHN4fcNs+ezICNLUM
bKGKfKX0j//U2K0X1S0E0T9YgOKBWYi+wONGkyT+kL0mojAt6JcmVzWJdJYY9hXi
ryQZVgICsroPFOrGimbBhkVVi76SvpykBMdJPJ7oKXqJ1/6v/2j1pReQvayZzKWG
VwlnRtvWFsJG8eSpUPWP0ZIV018+xgBJOm5YstHRJw0lyDL4IBHNfTIzSJRUTN3c
ecQwn+uOuFW114hcxWokPbLTBQNRxgfvzBRydD1ucs4YKIxKoHflCStFREest2d/
AYoFWpO+ocH/+OcOZ6RHSXZddZAa9SaP8A==
-----END CERTIFICATE-----

# Issuer: CN=NetLock Kozjegyzoi (Class A) Tanusitvanykiado O=NetLock Halozatbiztonsagi Kft. OU=Tanusitvanykiadok
# Subject: CN=NetLock Kozjegyzoi (Class A) Tanusitvanykiado O=NetLock Halozatbiztonsagi Kft. OU=Tanusitvanykiadok
# Label: "NetLock Notary (Class A) Root"
# Serial: 259
# MD5 Fingerprint: 86:38:6d:5e:49:63:6c:85:5c:db:6d:dc:94:b7:d0:f7
# SHA1 Fingerprint: ac:ed:5f:65:53:fd:25:ce:01:5f:1f:7a:48:3b:6a:74:9f:61:78:c6
# SHA256 Fingerprint: 7f:12:cd:5f:7e:5e:29:0e:c7:d8:51:79:d5:b7:2c:20:a5:be:75:08:ff:db:5b:f8:1a:b9:68:4a:7f:c9:f6:67
-----BEGIN CERTIFICATE-----
MIIGfTCCBWWgAwIBAgICAQMwDQYJKoZIhvcNAQEEBQAwga8xCzAJBgNVBAYTAkhV
MRAwDgYDVQQIEwdIdW5nYXJ5MREwDwYDVQQHEwhCdWRhcGVzdDEnMCUGA1UEChMe
TmV0TG9jayBIYWxvemF0Yml6dG9uc2FnaSBLZnQuMRowGAYDVQQLExFUYW51c2l0
dmFueWtpYWRvazE2MDQGA1UEAxMtTmV0TG9jayBLb3pqZWd5em9pIChDbGFzcyBB
KSBUYW51c2l0dmFueWtpYWRvMB4XDTk5MDIyNDIzMTQ0N1oXDTE5MDIxOTIzMTQ0
N1owga8xCzAJBgNVBAYTAkhVMRAwDgYDVQQIEwdIdW5nYXJ5MREwDwYDVQQHEwhC
dWRhcGVzdDEnMCUGA1UEChMeTmV0TG9jayBIYWxvemF0Yml6dG9uc2FnaSBLZnQu
MRowGAYDVQQLExFUYW51c2l0dmFueWtpYWRvazE2MDQGA1UEAxMtTmV0TG9jayBL
b3pqZWd5em9pIChDbGFzcyBBKSBUYW51c2l0dmFueWtpYWRvMIIBIjANBgkqhkiG
9w0BAQEFAAOCAQ8AMIIBCgKCAQEAvHSMD7tM9DceqQWC2ObhbHDqeLVu0ThEDaiD
zl3S1tWBxdRL51uUcCbbO51qTGL3cfNk1mE7PetzozfZz+qMkjvN9wfcZnSX9EUi
3fRc4L9t875lM+QVOr/bmJBVOMTtplVjC7B4BPTjbsE/jvxReB+SnoPC/tmwqcm8
WgD/qaiYdPv2LD4VOQ22BFWoDpggQrOxJa1+mm9dU7GrDPzr4PN6s6iz/0b2Y6LY
Oph7tqyF/7AlT3Rj5xMHpQqPBffAZG9+pyeAlt7ULoZgx2srXnN7F+eRP2QM2Esi
NCubMvJIH5+hCoR64sKtlz2O1cH5VqNQ6ca0+pii7pXmKgOM3wIDAQABo4ICnzCC
ApswDgYDVR0PAQH/BAQDAgAGMBIGA1UdEwEB/wQIMAYBAf8CAQQwEQYJYIZIAYb4
QgEBBAQDAgAHMIICYAYJYIZIAYb4QgENBIICURaCAk1GSUdZRUxFTSEgRXplbiB0
YW51c2l0dmFueSBhIE5ldExvY2sgS2Z0LiBBbHRhbGFub3MgU3pvbGdhbHRhdGFz
aSBGZWx0ZXRlbGVpYmVuIGxlaXJ0IGVsamFyYXNvayBhbGFwamFuIGtlc3p1bHQu
IEEgaGl0ZWxlc2l0ZXMgZm9seWFtYXRhdCBhIE5ldExvY2sgS2Z0LiB0ZXJtZWtm
ZWxlbG9zc2VnLWJpenRvc2l0YXNhIHZlZGkuIEEgZGlnaXRhbGlzIGFsYWlyYXMg
ZWxmb2dhZGFzYW5hayBmZWx0ZXRlbGUgYXogZWxvaXJ0IGVsbGVub3J6ZXNpIGVs
amFyYXMgbWVndGV0ZWxlLiBBeiBlbGphcmFzIGxlaXJhc2EgbWVndGFsYWxoYXRv
IGEgTmV0TG9jayBLZnQuIEludGVybmV0IGhvbmxhcGphbiBhIGh0dHBzOi8vd3d3
Lm5ldGxvY2submV0L2RvY3MgY2ltZW4gdmFneSBrZXJoZXRvIGF6IGVsbGVub3J6
ZXNAbmV0bG9jay5uZXQgZS1tYWlsIGNpbWVuLiBJTVBPUlRBTlQhIFRoZSBpc3N1
YW5jZSBhbmQgdGhlIHVzZSBvZiB0aGlzIGNlcnRpZmljYXRlIGlzIHN1YmplY3Qg
dG8gdGhlIE5ldExvY2sgQ1BTIGF2YWlsYWJsZSBhdCBodHRwczovL3d3dy5uZXRs
b2NrLm5ldC9kb2NzIG9yIGJ5IGUtbWFpbCBhdCBjcHNAbmV0bG9jay5uZXQuMA0G
CSqGSIb3DQEBBAUAA4IBAQBIJEb3ulZv+sgoA0BO5TE5ayZrU3/b39/zcT0mwBQO
xmd7I6gMc90Bu8bKbjc5VdXHjFYgDigKDtIqpLBJUsY4B/6+CgmM0ZjPytoUMaFP
0jn8DxEsQ8Pdq5PHVT5HfBgaANzze9jyf1JsIPQLX2lS9O74silg6+NJMSEN1rUQ
QeJBCWziGppWS3cC9qCbmieH6FUpccKQn0V4GuEVZD3QDtigdp+uxdAu6tYPVuxk
f1qbFFgBJ34TUMdrKuZoPL9coAob4Q566eKAw+np9v1sEZ7Q5SgnK1QyQhSCdeZK
8CtmdWOMovsEPoMOmzbwGOQmIMOM8CgHrTwXZoi1/baI
-----END CERTIFICATE-----

# Issuer: CN=XRamp Global Certification Authority O=XRamp Security Services Inc OU=www.xrampsecurity.com
# Subject: CN=XRamp Global Certification Authority O=XRamp Security Services Inc OU=www.xrampsecurity.com
# Label: "XRamp Global CA Root"
# Serial: 107108908803651509692980124233745014957
# MD5 Fingerprint: a1:0b:44:b3:ca:10:d8:00:6e:9d:0f:d8:0f:92:0a:d1
# SHA1 Fingerprint: b8:01:86:d1:eb:9c:86:a5:41:04:cf:30:54:f3:4c:52:b7:e5:58:c6
# SHA256 Fingerprint: ce:cd:dc:90:50:99:d8:da:df:c5:b1:d2:09:b7:37:cb:e2:c1:8c:fb:2c:10:c0:ff:0b:cf:0d:32:86:fc:1a:a2
-----BEGIN CERTIFICATE-----
MIIEMDCCAxigAwIBAgIQUJRs7Bjq1ZxN1ZfvdY+grTANBgkqhkiG9w0BAQUFADCB
gjELMAkGA1UEBhMCVVMxHjAcBgNVBAsTFXd3dy54cmFtcHNlY3VyaXR5LmNvbTEk
MCIGA1UEChMbWFJhbXAgU2VjdXJpdHkgU2VydmljZXMgSW5jMS0wKwYDVQQDEyRY
UmFtcCBHbG9iYWwgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwHhcNMDQxMTAxMTcx
NDA0WhcNMzUwMTAxMDUzNzE5WjCBgjELMAkGA1UEBhMCVVMxHjAcBgNVBAsTFXd3
dy54cmFtcHNlY3VyaXR5LmNvbTEkMCIGA1UEChMbWFJhbXAgU2VjdXJpdHkgU2Vy
dmljZXMgSW5jMS0wKwYDVQQDEyRYUmFtcCBHbG9iYWwgQ2VydGlmaWNhdGlvbiBB
dXRob3JpdHkwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCYJB69FbS6
38eMpSe2OAtp87ZOqCwuIR1cRN8hXX4jdP5efrRKt6atH67gBhbim1vZZ3RrXYCP
KZ2GG9mcDZhtdhAoWORlsH9KmHmf4MMxfoArtYzAQDsRhtDLooY2YKTVMIJt2W7Q
DxIEM5dfT2Fa8OT5kavnHTu86M/0ay00fOJIYRyO82FEzG+gSqmUsE3a56k0enI4
qEHMPJQRfevIpoy3hsvKMzvZPTeL+3o+hiznc9cKV6xkmxnr9A8ECIqsAxcZZPRa
JSKNNCyy9mgdEm3Tih4U2sSPpuIjhdV6Db1q4Ons7Be7QhtnqiXtRYMh/MHJfNVi
PvryxS3T/dRlAgMBAAGjgZ8wgZwwEwYJKwYBBAGCNxQCBAYeBABDAEEwCwYDVR0P
BAQDAgGGMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFMZPoj0GY4QJnM5i5ASs
jVy16bYbMDYGA1UdHwQvMC0wK6ApoCeGJWh0dHA6Ly9jcmwueHJhbXBzZWN1cml0
eS5jb20vWEdDQS5jcmwwEAYJKwYBBAGCNxUBBAMCAQEwDQYJKoZIhvcNAQEFBQAD
ggEBAJEVOQMBG2f7Shz5CmBbodpNl2L5JFMn14JkTpAuw0kbK5rc/Kh4ZzXxHfAR
vbdI4xD2Dd8/0sm2qlWkSLoC295ZLhVbO50WfUfXN+pfTXYSNrsf16GBBEYgoyxt
qZ4Bfj8pzgCT3/3JknOJiWSe5yvkHJEs0rnOfc5vMZnT5r7SHpDwCRR5XCOrTdLa
IR9NmXmd4c8nnxCbHIgNsIpkQTG4DmyQJKSbXHGPurt+HBvbaoAPIbzp26a3QPSy
i6mx5O+aGtA9aZnuqCij4Tyz8LIRnM98QObd50N9otg6tamN8jSZxNQQ4Qb9CYQQ
O+7ETPTsJ3xCwnR8gooJybQDJbw=
-----END CERTIFICATE-----

# Issuer: O=The Go Daddy Group, Inc. OU=Go Daddy Class 2 Certification Authority
# Subject: O=The Go Daddy Group, Inc. OU=Go Daddy Class 2 Certification Authority
# Label: "Go Daddy Class 2 CA"
# Serial: 0
# MD5 Fingerprint: 91:de:06:25:ab:da:fd:32:17:0c:bb:25:17:2a:84:67
# SHA1 Fingerprint: 27:96:ba:e6:3f:18:01:e2:77:26:1b:a0:d7:77:70:02:8f:20:ee:e4
# SHA256 Fingerprint: c3:84:6b:f2:4b:9e:93:ca:64:27:4c:0e:c6:7c:1e:cc:5e:02:4f:fc:ac:d2:d7:40:19:35:0e:81:fe:54:6a:e4
-----BEGIN CERTIFICATE-----
MIIEADCCAuigAwIBAgIBADANBgkqhkiG9w0BAQUFADBjMQswCQYDVQQGEwJVUzEh
MB8GA1UEChMYVGhlIEdvIERhZGR5IEdyb3VwLCBJbmMuMTEwLwYDVQQLEyhHbyBE
YWRkeSBDbGFzcyAyIENlcnRpZmljYXRpb24gQXV0aG9yaXR5MB4XDTA0MDYyOTE3
MDYyMFoXDTM0MDYyOTE3MDYyMFowYzELMAkGA1UEBhMCVVMxITAfBgNVBAoTGFRo
ZSBHbyBEYWRkeSBHcm91cCwgSW5jLjExMC8GA1UECxMoR28gRGFkZHkgQ2xhc3Mg
MiBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTCCASAwDQYJKoZIhvcNAQEBBQADggEN
ADCCAQgCggEBAN6d1+pXGEmhW+vXX0iG6r7d/+TvZxz0ZWizV3GgXne77ZtJ6XCA
PVYYYwhv2vLM0D9/AlQiVBDYsoHUwHU9S3/Hd8M+eKsaA7Ugay9qK7HFiH7Eux6w
wdhFJ2+qN1j3hybX2C32qRe3H3I2TqYXP2WYktsqbl2i/ojgC95/5Y0V4evLOtXi
EqITLdiOr18SPaAIBQi2XKVlOARFmR6jYGB0xUGlcmIbYsUfb18aQr4CUWWoriMY
avx4A6lNf4DD+qta/KFApMoZFv6yyO9ecw3ud72a9nmYvLEHZ6IVDd2gWMZEewo+
YihfukEHU1jPEX44dMX4/7VpkI+EdOqXG68CAQOjgcAwgb0wHQYDVR0OBBYEFNLE
sNKR1EwRcbNhyz2h/t2oatTjMIGNBgNVHSMEgYUwgYKAFNLEsNKR1EwRcbNhyz2h
/t2oatTjoWekZTBjMQswCQYDVQQGEwJVUzEhMB8GA1UEChMYVGhlIEdvIERhZGR5
IEdyb3VwLCBJbmMuMTEwLwYDVQQLEyhHbyBEYWRkeSBDbGFzcyAyIENlcnRpZmlj
YXRpb24gQXV0aG9yaXR5ggEAMAwGA1UdEwQFMAMBAf8wDQYJKoZIhvcNAQEFBQAD
ggEBADJL87LKPpH8EsahB4yOd6AzBhRckB4Y9wimPQoZ+YeAEW5p5JYXMP80kWNy
OO7MHAGjHZQopDH2esRU1/blMVgDoszOYtuURXO1v0XJJLXVggKtI3lpjbi2Tc7P
TMozI+gciKqdi0FuFskg5YmezTvacPd+mSYgFFQlq25zheabIZ0KbIIOqPjCDPoQ
HmyW74cNxA9hi63ugyuV+I6ShHI56yDqg+2DzZduCLzrTia2cyvk0/ZM/iZx4mER
dEr/VxqHD3VILs9RaRegAhJhldXRQLIQTO7ErBBDpqWeCtWVYpoNz4iCxTIM5Cuf
ReYNnyicsbkqWletNw+vHX/bvZ8=
-----END CERTIFICATE-----

# Issuer: O=Starfield Technologies, Inc. OU=Starfield Class 2 Certification Authority
# Subject: O=Starfield Technologies, Inc. OU=Starfield Class 2 Certification Authority
# Label: "Starfield Class 2 CA"
# Serial: 0
# MD5 Fingerprint: 32:4a:4b:bb:c8:63:69:9b:be:74:9a:c6:dd:1d:46:24
# SHA1 Fingerprint: ad:7e:1c:28:b0:64:ef:8f:60:03:40:20:14:c3:d0:e3:37:0e:b5:8a
# SHA256 Fingerprint: 14:65:fa:20:53:97:b8:76:fa:a6:f0:a9:95:8e:55:90:e4:0f:cc:7f:aa:4f:b7:c2:c8:67:75:21:fb:5f:b6:58
-----BEGIN CERTIFICATE-----
MIIEDzCCAvegAwIBAgIBADANBgkqhkiG9w0BAQUFADBoMQswCQYDVQQGEwJVUzEl
MCMGA1UEChMcU3RhcmZpZWxkIFRlY2hub2xvZ2llcywgSW5jLjEyMDAGA1UECxMp
U3RhcmZpZWxkIENsYXNzIDIgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwHhcNMDQw
NjI5MTczOTE2WhcNMzQwNjI5MTczOTE2WjBoMQswCQYDVQQGEwJVUzElMCMGA1UE
ChMcU3RhcmZpZWxkIFRlY2hub2xvZ2llcywgSW5jLjEyMDAGA1UECxMpU3RhcmZp
ZWxkIENsYXNzIDIgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwggEgMA0GCSqGSIb3
DQEBAQUAA4IBDQAwggEIAoIBAQC3Msj+6XGmBIWtDBFk385N78gDGIc/oav7PKaf
8MOh2tTYbitTkPskpD6E8J7oX+zlJ0T1KKY/e97gKvDIr1MvnsoFAZMej2YcOadN
+lq2cwQlZut3f+dZxkqZJRRU6ybH838Z1TBwj6+wRir/resp7defqgSHo9T5iaU0
X9tDkYI22WY8sbi5gv2cOj4QyDvvBmVmepsZGD3/cVE8MC5fvj13c7JdBmzDI1aa
K4UmkhynArPkPw2vCHmCuDY96pzTNbO8acr1zJ3o/WSNF4Azbl5KXZnJHoe0nRrA
1W4TNSNe35tfPe/W93bC6j67eA0cQmdrBNj41tpvi/JEoAGrAgEDo4HFMIHCMB0G
A1UdDgQWBBS/X7fRzt0fhvRbVazc1xDCDqmI5zCBkgYDVR0jBIGKMIGHgBS/X7fR
zt0fhvRbVazc1xDCDqmI56FspGowaDELMAkGA1UEBhMCVVMxJTAjBgNVBAoTHFN0
YXJmaWVsZCBUZWNobm9sb2dpZXMsIEluYy4xMjAwBgNVBAsTKVN0YXJmaWVsZCBD
bGFzcyAyIENlcnRpZmljYXRpb24gQXV0aG9yaXR5ggEAMAwGA1UdEwQFMAMBAf8w
DQYJKoZIhvcNAQEFBQADggEBAAWdP4id0ckaVaGsafPzWdqbAYcaT1epoXkJKtv3
L7IezMdeatiDh6GX70k1PncGQVhiv45YuApnP+yz3SFmH8lU+nLMPUxA2IGvd56D
eruix/U0F47ZEUD0/CwqTRV/p2JdLiXTAAsgGh1o+Re49L2L7ShZ3U0WixeDyLJl
xy16paq8U4Zt3VekyvggQQto8PT7dL5WXXp59fkdheMtlb71cZBDzI0fmgAKhynp
VSJYACPq4xJDKVtHCN2MQWplBqjlIapBtJUhlbl90TSrE9atvNziPTnNvT51cKEY
WQPJIrSPnNVeKtelttQKbfi3QBFGmh95DmK/D5fs4C8fF5Q=
-----END CERTIFICATE-----

# Issuer: CN=StartCom Certification Authority O=StartCom Ltd. OU=Secure Digital Certificate Signing
# Subject: CN=StartCom Certification Authority O=StartCom Ltd. OU=Secure Digital Certificate Signing
# Label: "StartCom Certification Authority"
# Serial: 1
# MD5 Fingerprint: 22:4d:8f:8a:fc:f7:35:c2:bb:57:34:90:7b:8b:22:16
# SHA1 Fingerprint: 3e:2b:f7:f2:03:1b:96:f3:8c:e6:c4:d8:a8:5d:3e:2d:58:47:6a:0f
# SHA256 Fingerprint: c7:66:a9:be:f2:d4:07:1c:86:3a:31:aa:49:20:e8:13:b2:d1:98:60:8c:b7:b7:cf:e2:11:43:b8:36:df:09:ea
-----BEGIN CERTIFICATE-----
MIIHyTCCBbGgAwIBAgIBATANBgkqhkiG9w0BAQUFADB9MQswCQYDVQQGEwJJTDEW
MBQGA1UEChMNU3RhcnRDb20gTHRkLjErMCkGA1UECxMiU2VjdXJlIERpZ2l0YWwg
Q2VydGlmaWNhdGUgU2lnbmluZzEpMCcGA1UEAxMgU3RhcnRDb20gQ2VydGlmaWNh
dGlvbiBBdXRob3JpdHkwHhcNMDYwOTE3MTk0NjM2WhcNMzYwOTE3MTk0NjM2WjB9
MQswCQYDVQQGEwJJTDEWMBQGA1UEChMNU3RhcnRDb20gTHRkLjErMCkGA1UECxMi
U2VjdXJlIERpZ2l0YWwgQ2VydGlmaWNhdGUgU2lnbmluZzEpMCcGA1UEAxMgU3Rh
cnRDb20gQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwggIiMA0GCSqGSIb3DQEBAQUA
A4ICDwAwggIKAoICAQDBiNsJvGxGfHiflXu1M5DycmLWwTYgIiRezul38kMKogZk
pMyONvg45iPwbm2xPN1yo4UcodM9tDMr0y+v/uqwQVlntsQGfQqedIXWeUyAN3rf
OQVSWff0G0ZDpNKFhdLDcfN1YjS6LIp/Ho/u7TTQEceWzVI9ujPW3U3eCztKS5/C
Ji/6tRYccjV3yjxd5srhJosaNnZcAdt0FCX+7bWgiA/deMotHweXMAEtcnn6RtYT
Kqi5pquDSR3l8u/d5AGOGAqPY1MWhWKpDhk6zLVmpsJrdAfkK+F2PrRt2PZE4XNi
HzvEvqBTViVsUQn3qqvKv3b9bZvzndu/PWa8DFaqr5hIlTpL36dYUNk4dalb6kMM
Av+Z6+hsTXBbKWWc3apdzK8BMewM69KN6Oqce+Zu9ydmDBpI125C4z/eIT574Q1w
+2OqqGwaVLRcJXrJosmLFqa7LH4XXgVNWG4SHQHuEhANxjJ/GP/89PrNbpHoNkm+
Gkhpi8KWTRoSsmkXwQqQ1vp5Iki/untp+HDH+no32NgN0nZPV/+Qt+OR0t3vwmC3
Zzrd/qqc8NSLf3Iizsafl7b4r4qgEKjZ+xjGtrVcUjyJthkqcwEKDwOzEmDyei+B
26Nu/yYwl/WL3YlXtq09s68rxbd2AvCl1iuahhQqcvbjM4xdCUsT37uMdBNSSwID
AQABo4ICUjCCAk4wDAYDVR0TBAUwAwEB/zALBgNVHQ8EBAMCAa4wHQYDVR0OBBYE
FE4L7xqkQFulF2mHMMo0aEPQQa7yMGQGA1UdHwRdMFswLKAqoCiGJmh0dHA6Ly9j
ZXJ0LnN0YXJ0Y29tLm9yZy9zZnNjYS1jcmwuY3JsMCugKaAnhiVodHRwOi8vY3Js
LnN0YXJ0Y29tLm9yZy9zZnNjYS1jcmwuY3JsMIIBXQYDVR0gBIIBVDCCAVAwggFM
BgsrBgEEAYG1NwEBATCCATswLwYIKwYBBQUHAgEWI2h0dHA6Ly9jZXJ0LnN0YXJ0
Y29tLm9yZy9wb2xpY3kucGRmMDUGCCsGAQUFBwIBFilodHRwOi8vY2VydC5zdGFy
dGNvbS5vcmcvaW50ZXJtZWRpYXRlLnBkZjCB0AYIKwYBBQUHAgIwgcMwJxYgU3Rh
cnQgQ29tbWVyY2lhbCAoU3RhcnRDb20pIEx0ZC4wAwIBARqBl0xpbWl0ZWQgTGlh
YmlsaXR5LCByZWFkIHRoZSBzZWN0aW9uICpMZWdhbCBMaW1pdGF0aW9ucyogb2Yg
dGhlIFN0YXJ0Q29tIENlcnRpZmljYXRpb24gQXV0aG9yaXR5IFBvbGljeSBhdmFp
bGFibGUgYXQgaHR0cDovL2NlcnQuc3RhcnRjb20ub3JnL3BvbGljeS5wZGYwEQYJ
YIZIAYb4QgEBBAQDAgAHMDgGCWCGSAGG+EIBDQQrFilTdGFydENvbSBGcmVlIFNT
TCBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTANBgkqhkiG9w0BAQUFAAOCAgEAFmyZ
9GYMNPXQhV59CuzaEE44HF7fpiUFS5Eyweg78T3dRAlbB0mKKctmArexmvclmAk8
jhvh3TaHK0u7aNM5Zj2gJsfyOZEdUauCe37Vzlrk4gNXcGmXCPleWKYK34wGmkUW
FjgKXlf2Ysd6AgXmvB618p70qSmD+LIU424oh0TDkBreOKk8rENNZEXO3SipXPJz
ewT4F+irsfMuXGRuczE6Eri8sxHkfY+BUZo7jYn0TZNmezwD7dOaHZrzZVD1oNB1
ny+v8OqCQ5j4aZyJecRDjkZy42Q2Eq/3JR44iZB3fsNrarnDy0RLrHiQi+fHLB5L
EUTINFInzQpdn4XBidUaePKVEFMy3YCEZnXZtWgo+2EuvoSoOMCZEoalHmdkrQYu
L6lwhceWD3yJZfWOQ1QOq92lgDmUYMA0yZZwLKMS9R9Ie70cfmu3nZD0Ijuu+Pwq
yvqCUqDvr0tVk+vBtfAii6w0TiYiBKGHLHVKt+V9E9e4DGTANtLJL4YSjCMJwRuC
O3NJo2pXh5Tl1njFmUNj403gdy3hZZlyaQQaRwnmDwFWJPsfvw55qVguucQJAX6V
um0ABj6y6koQOdjQK/W/7HW/lwLFCRsI3FU34oH7N4RDYiDK51ZLZer+bMEkkySh
NOsF/5oirpt9P/FlUQqmMGqz9IgcgA38corog14=
-----END CERTIFICATE-----

# Issuer: O=Government Root Certification Authority
# Subject: O=Government Root Certification Authority
# Label: "Taiwan GRCA"
# Serial: 42023070807708724159991140556527066870
# MD5 Fingerprint: 37:85:44:53:32:45:1f:20:f0:f3:95:e1:25:c4:43:4e
# SHA1 Fingerprint: f4:8b:11:bf:de:ab:be:94:54:20:71:e6:41:de:6b:be:88:2b:40:b9
# SHA256 Fingerprint: 76:00:29:5e:ef:e8:5b:9e:1f:d6:24:db:76:06:2a:aa:ae:59:81:8a:54:d2:77:4c:d4:c0:b2:c0:11:31:e1:b3
-----BEGIN CERTIFICATE-----
MIIFcjCCA1qgAwIBAgIQH51ZWtcvwgZEpYAIaeNe9jANBgkqhkiG9w0BAQUFADA/
MQswCQYDVQQGEwJUVzEwMC4GA1UECgwnR292ZXJubWVudCBSb290IENlcnRpZmlj
YXRpb24gQXV0aG9yaXR5MB4XDTAyMTIwNTEzMjMzM1oXDTMyMTIwNTEzMjMzM1ow
PzELMAkGA1UEBhMCVFcxMDAuBgNVBAoMJ0dvdmVybm1lbnQgUm9vdCBDZXJ0aWZp
Y2F0aW9uIEF1dGhvcml0eTCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIB
AJoluOzMonWoe/fOW1mKydGGEghU7Jzy50b2iPN86aXfTEc2pBsBHH8eV4qNw8XR
IePaJD9IK/ufLqGU5ywck9G/GwGHU5nOp/UKIXZ3/6m3xnOUT0b3EEk3+qhZSV1q
gQdW8or5BtD3cCJNtLdBuTK4sfCxw5w/cP1T3YGq2GN49thTbqGsaoQkclSGxtKy
yhwOeYHWtXBiCAEuTk8O1RGvqa/lmr/czIdtJuTJV6L7lvnM4T9TjGxMfptTCAts
F/tnyMKtsc2AtJfcdgEWFelq16TheEfOhtX7MfP6Mb40qij7cEwdScevLJ1tZqa2
jWR+tSBqnTuBto9AAGdLiYa4zGX+FVPpBMHWXx1E1wovJ5pGfaENda1UhhXcSTvx
ls4Pm6Dso3pdvtUqdULle96ltqqvKKyskKw4t9VoNSZ63Pc78/1Fm9G7Q3hub/FC
VGqY8A2tl+lSXunVanLeavcbYBT0peS2cWeqH+riTcFCQP5nRhc4L0c/cZyu5SHK
YS1tB6iEfC3uUSXxY5Ce/eFXiGvviiNtsea9P63RPZYLhY3Naye7twWb7LuRqQoH
EgKXTiCQ8P8NHuJBO9NAOueNXdpm5AKwB1KYXA6OM5zCppX7VRluTI6uSw+9wThN
Xo+EHWbNxWCWtFJaBYmOlXqYwZE8lSOyDvR5tMl8wUohAgMBAAGjajBoMB0GA1Ud
DgQWBBTMzO/MKWCkO7GStjz6MmKPrCUVOzAMBgNVHRMEBTADAQH/MDkGBGcqBwAE
MTAvMC0CAQAwCQYFKw4DAhoFADAHBgVnKgMAAAQUA5vwIhP/lSg209yewDL7MTqK
UWUwDQYJKoZIhvcNAQEFBQADggIBAECASvomyc5eMN1PhnR2WPWus4MzeKR6dBcZ
TulStbngCnRiqmjKeKBMmo4sIy7VahIkv9Ro04rQ2JyftB8M3jh+Vzj8jeJPXgyf
qzvS/3WXy6TjZwj/5cAWtUgBfen5Cv8b5Wppv3ghqMKnI6mGq3ZW6A4M9hPdKmaK
ZEk9GhiHkASfQlK3T8v+R0F2Ne//AHY2RTKbxkaFXeIksB7jSJaYV0eUVXoPQbFE
JPPB/hprv4j9wabak2BegUqZIJxIZhm1AHlUD7gsL0u8qV1bYH+Mh6XgUmMqvtg7
hUAV/h62ZT/FS9p+tXo1KaMuephgIqP0fSdOLeq0dDzpD6QzDxARvBMB1uUO07+1
EqLhRSPAzAhuYbeJq4PjJB7mXQfnHyA+z2fI56wwbSdLaG5LKlwCCDTb+HbkZ6Mm
nD+iMsJKxYEYMRBWqoTvLQr/uB930r+lWKBi5NdLkXWNiYCYfm3LU05er/ayl4WX
udpVBrkk7tfGOB5jGxI7leFYrPLfhNVfmS8NVVvmONsuP3LpSIXLuykTjx44Vbnz
ssQwmSNOXfJIoRIM3BKQCZBUkQM8R+XVyWXgt0t97EfTsws+rZ7QdAAO671RrcDe
LMDDav7v3Aun+kbfYNucpllQdSNpc5Oy+fwC00fmcc4QAu4njIT/rEUNE1yDMuAl
pYYsfPQS
-----END CERTIFICATE-----

# Issuer: CN=Swisscom Root CA 1 O=Swisscom OU=Digital Certificate Services
# Subject: CN=Swisscom Root CA 1 O=Swisscom OU=Digital Certificate Services
# Label: "Swisscom Root CA 1"
# Serial: 122348795730808398873664200247279986742
# MD5 Fingerprint: f8:38:7c:77:88:df:2c:16:68:2e:c2:e2:52:4b:b8:f9
# SHA1 Fingerprint: 5f:3a:fc:0a:8b:64:f6:86:67:34:74:df:7e:a9:a2:fe:f9:fa:7a:51
# SHA256 Fingerprint: 21:db:20:12:36:60:bb:2e:d4:18:20:5d:a1:1e:e7:a8:5a:65:e2:bc:6e:55:b5:af:7e:78:99:c8:a2:66:d9:2e
-----BEGIN CERTIFICATE-----
MIIF2TCCA8GgAwIBAgIQXAuFXAvnWUHfV8w/f52oNjANBgkqhkiG9w0BAQUFADBk
MQswCQYDVQQGEwJjaDERMA8GA1UEChMIU3dpc3Njb20xJTAjBgNVBAsTHERpZ2l0
YWwgQ2VydGlmaWNhdGUgU2VydmljZXMxGzAZBgNVBAMTElN3aXNzY29tIFJvb3Qg
Q0EgMTAeFw0wNTA4MTgxMjA2MjBaFw0yNTA4MTgyMjA2MjBaMGQxCzAJBgNVBAYT
AmNoMREwDwYDVQQKEwhTd2lzc2NvbTElMCMGA1UECxMcRGlnaXRhbCBDZXJ0aWZp
Y2F0ZSBTZXJ2aWNlczEbMBkGA1UEAxMSU3dpc3Njb20gUm9vdCBDQSAxMIICIjAN
BgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEA0LmwqAzZuz8h+BvVM5OAFmUgdbI9
m2BtRsiMMW8Xw/qabFbtPMWRV8PNq5ZJkCoZSx6jbVfd8StiKHVFXqrWW/oLJdih
FvkcxC7mlSpnzNApbjyFNDhhSbEAn9Y6cV9Nbc5fuankiX9qUvrKm/LcqfmdmUc/
TilftKaNXXsLmREDA/7n29uj/x2lzZAeAR81sH8A25Bvxn570e56eqeqDFdvpG3F
EzuwpdntMhy0XmeLVNxzh+XTF3xmUHJd1BpYwdnP2IkCb6dJtDZd0KTeByy2dbco
kdaXvij1mB7qWybJvbCXc9qukSbraMH5ORXWZ0sKbU/Lz7DkQnGMU3nn7uHbHaBu
HYwadzVcFh4rUx80i9Fs/PJnB3r1re3WmquhsUvhzDdf/X/NTa64H5xD+SpYVUNF
vJbNcA78yeNmuk6NO4HLFWR7uZToXTNShXEuT46iBhFRyePLoW4xCGQMwtI89Tbo
19AOeCMgkckkKmUpWyL3Ic6DXqTz3kvTaI9GdVyDCW4pa8RwjPWd1yAv/0bSKzjC
L3UcPX7ape8eYIVpQtPM+GP+HkM5haa2Y0EQs3MevNP6yn0WR+Kn1dCjigoIlmJW
bjTb2QK5MHXjBNLnj8KwEUAKrNVxAmKLMb7dxiNYMUJDLXT5xp6mig/p/r+D5kNX
JLrvRjSq1xIBOO0CAwEAAaOBhjCBgzAOBgNVHQ8BAf8EBAMCAYYwHQYDVR0hBBYw
FDASBgdghXQBUwABBgdghXQBUwABMBIGA1UdEwEB/wQIMAYBAf8CAQcwHwYDVR0j
BBgwFoAUAyUv3m+CATpcLNwroWm1Z9SM0/0wHQYDVR0OBBYEFAMlL95vggE6XCzc
K6FptWfUjNP9MA0GCSqGSIb3DQEBBQUAA4ICAQA1EMvspgQNDQ/NwNurqPKIlwzf
ky9NfEBWMXrrpA9gzXrzvsMnjgM+pN0S734edAY8PzHyHHuRMSG08NBsl9Tpl7Ik
Vh5WwzW9iAUPWxAaZOHHgjD5Mq2eUCzneAXQMbFamIp1TpBcahQq4FJHgmDmHtqB
sfsUC1rxn9KVuj7QG9YVHaO+htXbD8BJZLsuUBlL0iT43R4HVtA4oJVwIHaM190e
3p9xxCPvgxNcoyQVTSlAPGrEqdi3pkSlDfTgnXceQHAm/NrZNuR55LU/vJtlvrsR
ls/bxig5OgjOR1tTWsWZ/l2p3e9M1MalrQLmjAcSHm8D0W+go/MpvRLHUKKwf4ip
mXeascClOS5cfGniLLDqN2qk4Vrh9VDlg++luyqI54zb/W1elxmofmZ1a3Hqv7HH
b6D0jqTsNFFbjCYDcKF31QESVwA12yPeDooomf2xEG9L/zgtYE4snOtnta1J7ksf
rK/7DZBaZmBwXarNeNQk7shBoJMBkpxqnvy5JMWzFYJ+vq6VK+uxwNrjAWALXmms
hFZhvnEX/h0TD/7Gh0Xp/jKgGg0TpJRVcaUWi7rKibCyx/yP2FS1k2Kdzs9Z+z0Y
zirLNRWCXf9UIltxUvu3yf5gmwBBZPCqKuy2QkPOiWaByIufOVQDJdMWNY6E0F/6
MBr1mmz0DlP5OlvRHA==
-----END CERTIFICATE-----

# Issuer: CN=DigiCert Assured ID Root CA O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert Assured ID Root CA O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert Assured ID Root CA"
# Serial: 17154717934120587862167794914071425081
# MD5 Fingerprint: 87:ce:0b:7b:2a:0e:49:00:e1:58:71:9b:37:a8:93:72
# SHA1 Fingerprint: 05:63:b8:63:0d:62:d7:5a:bb:c8:ab:1e:4b:df:b5:a8:99:b2:4d:43
# SHA256 Fingerprint: 3e:90:99:b5:01:5e:8f:48:6c:00:bc:ea:9d:11:1e:e7:21:fa:ba:35:5a:89:bc:f1:df:69:56:1e:3d:c6:32:5c
-----BEGIN CERTIFICATE-----
MIIDtzCCAp+gAwIBAgIQDOfg5RfYRv6P5WD8G/AwOTANBgkqhkiG9w0BAQUFADBl
MQswCQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3
d3cuZGlnaWNlcnQuY29tMSQwIgYDVQQDExtEaWdpQ2VydCBBc3N1cmVkIElEIFJv
b3QgQ0EwHhcNMDYxMTEwMDAwMDAwWhcNMzExMTEwMDAwMDAwWjBlMQswCQYDVQQG
EwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3d3cuZGlnaWNl
cnQuY29tMSQwIgYDVQQDExtEaWdpQ2VydCBBc3N1cmVkIElEIFJvb3QgQ0EwggEi
MA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCtDhXO5EOAXLGH87dg+XESpa7c
JpSIqvTO9SA5KFhgDPiA2qkVlTJhPLWxKISKityfCgyDF3qPkKyK53lTXDGEKvYP
mDI2dsze3Tyoou9q+yHyUmHfnyDXH+Kx2f4YZNISW1/5WBg1vEfNoTb5a3/UsDg+
wRvDjDPZ2C8Y/igPs6eD1sNuRMBhNZYW/lmci3Zt1/GiSw0r/wty2p5g0I6QNcZ4
VYcgoc/lbQrISXwxmDNsIumH0DJaoroTghHtORedmTpyoeb6pNnVFzF1roV9Iq4/
AUaG9ih5yLHa5FcXxH4cDrC0kqZWs72yl+2qp/C3xag/lRbQ/6GW6whfGHdPAgMB
AAGjYzBhMA4GA1UdDwEB/wQEAwIBhjAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQW
BBRF66Kv9JLLgjEtUYunpyGd823IDzAfBgNVHSMEGDAWgBRF66Kv9JLLgjEtUYun
pyGd823IDzANBgkqhkiG9w0BAQUFAAOCAQEAog683+Lt8ONyc3pklL/3cmbYMuRC
dWKuh+vy1dneVrOfzM4UKLkNl2BcEkxY5NM9g0lFWJc1aRqoR+pWxnmrEthngYTf
fwk8lOa4JiwgvT2zKIn3X/8i4peEH+ll74fg38FnSbNd67IJKusm7Xi+fT8r87cm
NW1fiQG2SVufAQWbqz0lwcy2f8Lxb4bG+mRo64EtlOtCt/qMHt1i8b5QZ7dsvfPx
H2sMNgcWfzd8qVttevESRmCD1ycEvkvOl77DZypoEd+A5wwzZr8TDRRu838fYxAe
+o0bJW1sj6W3YQGx0qMmoRBxna3iw/nDmVG3KwcIzi7mULKn+gpFL6Lw8g==
-----END CERTIFICATE-----

# Issuer: CN=DigiCert Global Root CA O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert Global Root CA O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert Global Root CA"
# Serial: 10944719598952040374951832963794454346
# MD5 Fingerprint: 79:e4:a9:84:0d:7d:3a:96:d7:c0:4f:e2:43:4c:89:2e
# SHA1 Fingerprint: a8:98:5d:3a:65:e5:e5:c4:b2:d7:d6:6d:40:c6:dd:2f:b1:9c:54:36
# SHA256 Fingerprint: 43:48:a0:e9:44:4c:78:cb:26:5e:05:8d:5e:89:44:b4:d8:4f:96:62:bd:26:db:25:7f:89:34:a4:43:c7:01:61
-----BEGIN CERTIFICATE-----
MIIDrzCCApegAwIBAgIQCDvgVpBCRrGhdWrJWZHHSjANBgkqhkiG9w0BAQUFADBh
MQswCQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3
d3cuZGlnaWNlcnQuY29tMSAwHgYDVQQDExdEaWdpQ2VydCBHbG9iYWwgUm9vdCBD
QTAeFw0wNjExMTAwMDAwMDBaFw0zMTExMTAwMDAwMDBaMGExCzAJBgNVBAYTAlVT
MRUwEwYDVQQKEwxEaWdpQ2VydCBJbmMxGTAXBgNVBAsTEHd3dy5kaWdpY2VydC5j
b20xIDAeBgNVBAMTF0RpZ2lDZXJ0IEdsb2JhbCBSb290IENBMIIBIjANBgkqhkiG
9w0BAQEFAAOCAQ8AMIIBCgKCAQEA4jvhEXLeqKTTo1eqUKKPC3eQyaKl7hLOllsB
CSDMAZOnTjC3U/dDxGkAV53ijSLdhwZAAIEJzs4bg7/fzTtxRuLWZscFs3YnFo97
nh6Vfe63SKMI2tavegw5BmV/Sl0fvBf4q77uKNd0f3p4mVmFaG5cIzJLv07A6Fpt
43C/dxC//AH2hdmoRBBYMql1GNXRor5H4idq9Joz+EkIYIvUX7Q6hL+hqkpMfT7P
T19sdl6gSzeRntwi5m3OFBqOasv+zbMUZBfHWymeMr/y7vrTC0LUq7dBMtoM1O/4
gdW7jVg/tRvoSSiicNoxBN33shbyTApOB6jtSj1etX+jkMOvJwIDAQABo2MwYTAO
BgNVHQ8BAf8EBAMCAYYwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQUA95QNVbR
TLtm8KPiGxvDl7I90VUwHwYDVR0jBBgwFoAUA95QNVbRTLtm8KPiGxvDl7I90VUw
DQYJKoZIhvcNAQEFBQADggEBAMucN6pIExIK+t1EnE9SsPTfrgT1eXkIoyQY/Esr
hMAtudXH/vTBH1jLuG2cenTnmCmrEbXjcKChzUyImZOMkXDiqw8cvpOp/2PV5Adg
06O/nVsJ8dWO41P0jmP6P6fbtGbfYmbW0W5BjfIttep3Sp+dWOIrWcBAI+0tKIJF
PnlUkiaY4IBIqDfv8NZ5YBberOgOzW6sRBc4L0na4UU+Krk2U886UAb3LujEV0ls
YSEY1QSteDwsOoBrp+uvFRTp2InBuThs4pFsiv9kuXclVzDAGySj4dzp30d8tbQk
CAUw7C29C79Fv1C5qfPrmAESrciIxpg0X40KPMbp1ZWVbd4=
-----END CERTIFICATE-----

# Issuer: CN=DigiCert High Assurance EV Root CA O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert High Assurance EV Root CA O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert High Assurance EV Root CA"
# Serial: 3553400076410547919724730734378100087
# MD5 Fingerprint: d4:74:de:57:5c:39:b2:d3:9c:85:83:c5:c0:65:49:8a
# SHA1 Fingerprint: 5f:b7:ee:06:33:e2:59:db:ad:0c:4c:9a:e6:d3:8f:1a:61:c7:dc:25
# SHA256 Fingerprint: 74:31:e5:f4:c3:c1:ce:46:90:77:4f:0b:61:e0:54:40:88:3b:a9:a0:1e:d0:0b:a6:ab:d7:80:6e:d3:b1:18:cf
-----BEGIN CERTIFICATE-----
MIIDxTCCAq2gAwIBAgIQAqxcJmoLQJuPC3nyrkYldzANBgkqhkiG9w0BAQUFADBs
MQswCQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3
d3cuZGlnaWNlcnQuY29tMSswKQYDVQQDEyJEaWdpQ2VydCBIaWdoIEFzc3VyYW5j
ZSBFViBSb290IENBMB4XDTA2MTExMDAwMDAwMFoXDTMxMTExMDAwMDAwMFowbDEL
MAkGA1UEBhMCVVMxFTATBgNVBAoTDERpZ2lDZXJ0IEluYzEZMBcGA1UECxMQd3d3
LmRpZ2ljZXJ0LmNvbTErMCkGA1UEAxMiRGlnaUNlcnQgSGlnaCBBc3N1cmFuY2Ug
RVYgUm9vdCBDQTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAMbM5XPm
+9S75S0tMqbf5YE/yc0lSbZxKsPVlDRnogocsF9ppkCxxLeyj9CYpKlBWTrT3JTW
PNt0OKRKzE0lgvdKpVMSOO7zSW1xkX5jtqumX8OkhPhPYlG++MXs2ziS4wblCJEM
xChBVfvLWokVfnHoNb9Ncgk9vjo4UFt3MRuNs8ckRZqnrG0AFFoEt7oT61EKmEFB
Ik5lYYeBQVCmeVyJ3hlKV9Uu5l0cUyx+mM0aBhakaHPQNAQTXKFx01p8VdteZOE3
hzBWBOURtCmAEvF5OYiiAhF8J2a3iLd48soKqDirCmTCv2ZdlYTBoSUeh10aUAsg
EsxBu24LUTi4S8sCAwEAAaNjMGEwDgYDVR0PAQH/BAQDAgGGMA8GA1UdEwEB/wQF
MAMBAf8wHQYDVR0OBBYEFLE+w2kD+L9HAdSYJhoIAu9jZCvDMB8GA1UdIwQYMBaA
FLE+w2kD+L9HAdSYJhoIAu9jZCvDMA0GCSqGSIb3DQEBBQUAA4IBAQAcGgaX3Nec
nzyIZgYIVyHbIUf4KmeqvxgydkAQV8GK83rZEWWONfqe/EW1ntlMMUu4kehDLI6z
eM7b41N5cdblIZQB2lWHmiRk9opmzN6cN82oNLFpmyPInngiK3BD41VHMWEZ71jF
hS9OMPagMRYjyOfiZRYzy78aG6A9+MpeizGLYAiJLQwGXFK3xPkKmNEVX58Svnw2
Yzi9RKR/5CYrCsSXaQ3pjOLAEFe4yHYSkVXySGnYvCoCWw9E1CAx2/S6cCZdkGCe
vEsXCS+0yx5DaMkHJ8HSXPfqIbloEpw8nL+e/IBcm2PN7EeqJSdnoDfzAIJ9VNep
+OkuE6N36B9K
-----END CERTIFICATE-----

# Issuer: CN=Class 2 Primary CA O=Certplus
# Subject: CN=Class 2 Primary CA O=Certplus
# Label: "Certplus Class 2 Primary CA"
# Serial: 177770208045934040241468760488327595043
# MD5 Fingerprint: 88:2c:8c:52:b8:a2:3c:f3:f7:bb:03:ea:ae:ac:42:0b
# SHA1 Fingerprint: 74:20:74:41:72:9c:dd:92:ec:79:31:d8:23:10:8d:c2:81:92:e2:bb
# SHA256 Fingerprint: 0f:99:3c:8a:ef:97:ba:af:56:87:14:0e:d5:9a:d1:82:1b:b4:af:ac:f0:aa:9a:58:b5:d5:7a:33:8a:3a:fb:cb
-----BEGIN CERTIFICATE-----
MIIDkjCCAnqgAwIBAgIRAIW9S/PY2uNp9pTXX8OlRCMwDQYJKoZIhvcNAQEFBQAw
PTELMAkGA1UEBhMCRlIxETAPBgNVBAoTCENlcnRwbHVzMRswGQYDVQQDExJDbGFz
cyAyIFByaW1hcnkgQ0EwHhcNOTkwNzA3MTcwNTAwWhcNMTkwNzA2MjM1OTU5WjA9
MQswCQYDVQQGEwJGUjERMA8GA1UEChMIQ2VydHBsdXMxGzAZBgNVBAMTEkNsYXNz
IDIgUHJpbWFyeSBDQTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBANxQ
ltAS+DXSCHh6tlJw/W/uz7kRy1134ezpfgSN1sxvc0NXYKwzCkTsA18cgCSR5aiR
VhKC9+Ar9NuuYS6JEI1rbLqzAr3VNsVINyPi8Fo3UjMXEuLRYE2+L0ER4/YXJQyL
kcAbmXuZVg2v7tK8R1fjeUl7NIknJITesezpWE7+Tt9avkGtrAjFGA7v0lPubNCd
EgETjdyAYveVqUSISnFOYFWe2yMZeVYHDD9jC1yw4r5+FfyUM1hBOHTE4Y+L3yas
H7WLO7dDWWuwJKZtkIvEcupdM5i3y95ee++U8Rs+yskhwcWYAqqi9lt3m/V+llU0
HGdpwPFC40es/CgcZlUCAwEAAaOBjDCBiTAPBgNVHRMECDAGAQH/AgEKMAsGA1Ud
DwQEAwIBBjAdBgNVHQ4EFgQU43Mt38sOKAze3bOkynm4jrvoMIkwEQYJYIZIAYb4
QgEBBAQDAgEGMDcGA1UdHwQwMC4wLKAqoCiGJmh0dHA6Ly93d3cuY2VydHBsdXMu
Y29tL0NSTC9jbGFzczIuY3JsMA0GCSqGSIb3DQEBBQUAA4IBAQCnVM+IRBnL39R/
AN9WM2K191EBkOvDP9GIROkkXe/nFL0gt5o8AP5tn9uQ3Nf0YtaLcF3n5QRIqWh8
yfFC82x/xXp8HVGIutIKPidd3i1RTtMTZGnkLuPT55sJmabglZvOGtd/vjzOUrMR
FcEPF80Du5wlFbqidon8BvEY0JNLDnyCt6X09l/+7UCmnYR0ObncHoUW2ikbhiMA
ybuJfm6AiB4vFLQDJKgybwOaRywwvlbGp0ICcBvqQNi6BQNwB6SW//1IMwrh3KWB
kJtN3X3n57LNXMhqlfil9o3EXXgIvnsG1knPGTZQIy4I5p4FTUcY1Rbpsda2ENW7
l7+ijrRU
-----END CERTIFICATE-----

# Issuer: CN=DST Root CA X3 O=Digital Signature Trust Co.
# Subject: CN=DST Root CA X3 O=Digital Signature Trust Co.
# Label: "DST Root CA X3"
# Serial: 91299735575339953335919266965803778155
# MD5 Fingerprint: 41:03:52:dc:0f:f7:50:1b:16:f0:02:8e:ba:6f:45:c5
# SHA1 Fingerprint: da:c9:02:4f:54:d8:f6:df:94:93:5f:b1:73:26:38:ca:6a:d7:7c:13
# SHA256 Fingerprint: 06:87:26:03:31:a7:24:03:d9:09:f1:05:e6:9b:cf:0d:32:e1:bd:24:93:ff:c6:d9:20:6d:11:bc:d6:77:07:39
-----BEGIN CERTIFICATE-----
MIIDSjCCAjKgAwIBAgIQRK+wgNajJ7qJMDmGLvhAazANBgkqhkiG9w0BAQUFADA/
MSQwIgYDVQQKExtEaWdpdGFsIFNpZ25hdHVyZSBUcnVzdCBDby4xFzAVBgNVBAMT
DkRTVCBSb290IENBIFgzMB4XDTAwMDkzMDIxMTIxOVoXDTIxMDkzMDE0MDExNVow
PzEkMCIGA1UEChMbRGlnaXRhbCBTaWduYXR1cmUgVHJ1c3QgQ28uMRcwFQYDVQQD
Ew5EU1QgUm9vdCBDQSBYMzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEB
AN+v6ZdQCINXtMxiZfaQguzH0yxrMMpb7NnDfcdAwRgUi+DoM3ZJKuM/IUmTrE4O
rz5Iy2Xu/NMhD2XSKtkyj4zl93ewEnu1lcCJo6m67XMuegwGMoOifooUMM0RoOEq
OLl5CjH9UL2AZd+3UWODyOKIYepLYYHsUmu5ouJLGiifSKOeDNoJjj4XLh7dIN9b
xiqKqy69cK3FCxolkHRyxXtqqzTWMIn/5WgTe1QLyNau7Fqckh49ZLOMxt+/yUFw
7BZy1SbsOFU5Q9D8/RhcQPGX69Wam40dutolucbY38EVAjqr2m7xPi71XAicPNaD
aeQQmxkqtilX4+U9m5/wAl0CAwEAAaNCMEAwDwYDVR0TAQH/BAUwAwEB/zAOBgNV
HQ8BAf8EBAMCAQYwHQYDVR0OBBYEFMSnsaR7LHH62+FLkHX/xBVghYkQMA0GCSqG
SIb3DQEBBQUAA4IBAQCjGiybFwBcqR7uKGY3Or+Dxz9LwwmglSBd49lZRNI+DT69
ikugdB/OEIKcdBodfpga3csTS7MgROSR6cz8faXbauX+5v3gTt23ADq1cEmv8uXr
AvHRAosZy5Q6XkjEGB5YGV8eAlrwDPGxrancWYaLbumR9YbK+rlmM6pZW87ipxZz
R8srzJmwN0jP41ZL9c8PDHIyh8bwRLtTcm1D9SZImlJnt1ir/md2cXjbDaJWFBM5
JDGFoqgCWjBH4d1QB7wCCZAA62RjYJsWvIjJEubSfZGL+T0yjWW06XyxV3bqxbYo
Ob8VZRzI9neWagqNdwvYkQsEjgfbKbYK7p2CNTUQ
-----END CERTIFICATE-----

# Issuer: CN=DST ACES CA X6 O=Digital Signature Trust OU=DST ACES
# Subject: CN=DST ACES CA X6 O=Digital Signature Trust OU=DST ACES
# Label: "DST ACES CA X6"
# Serial: 17771143917277623872238992636097467865
# MD5 Fingerprint: 21:d8:4c:82:2b:99:09:33:a2:eb:14:24:8d:8e:5f:e8
# SHA1 Fingerprint: 40:54:da:6f:1c:3f:40:74:ac:ed:0f:ec:cd:db:79:d1:53:fb:90:1d
# SHA256 Fingerprint: 76:7c:95:5a:76:41:2c:89:af:68:8e:90:a1:c7:0f:55:6c:fd:6b:60:25:db:ea:10:41:6d:7e:b6:83:1f:8c:40
-----BEGIN CERTIFICATE-----
MIIECTCCAvGgAwIBAgIQDV6ZCtadt3js2AdWO4YV2TANBgkqhkiG9w0BAQUFADBb
MQswCQYDVQQGEwJVUzEgMB4GA1UEChMXRGlnaXRhbCBTaWduYXR1cmUgVHJ1c3Qx
ETAPBgNVBAsTCERTVCBBQ0VTMRcwFQYDVQQDEw5EU1QgQUNFUyBDQSBYNjAeFw0w
MzExMjAyMTE5NThaFw0xNzExMjAyMTE5NThaMFsxCzAJBgNVBAYTAlVTMSAwHgYD
VQQKExdEaWdpdGFsIFNpZ25hdHVyZSBUcnVzdDERMA8GA1UECxMIRFNUIEFDRVMx
FzAVBgNVBAMTDkRTVCBBQ0VTIENBIFg2MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A
MIIBCgKCAQEAuT31LMmU3HWKlV1j6IR3dma5WZFcRt2SPp/5DgO0PWGSvSMmtWPu
ktKe1jzIDZBfZIGxqAgNTNj50wUoUrQBJcWVHAx+PhCEdc/BGZFjz+iokYi5Q1K7
gLFViYsx+tC3dr5BPTCapCIlF3PoHuLTrCq9Wzgh1SpL11V94zpVvddtawJXa+ZH
fAjIgrrep4c9oW24MFbCswKBXy314powGCi4ZtPLAZZv6opFVdbgnf9nKxcCpk4a
ahELfrd755jWjHZvwTvbUJN+5dCOHze4vbrGn2zpfDPyMjwmR/onJALJfh1biEIT
ajV8fTXpLmaRcpPVMibEdPVTo7NdmvYJywIDAQABo4HIMIHFMA8GA1UdEwEB/wQF
MAMBAf8wDgYDVR0PAQH/BAQDAgHGMB8GA1UdEQQYMBaBFHBraS1vcHNAdHJ1c3Rk
c3QuY29tMGIGA1UdIARbMFkwVwYKYIZIAWUDAgEBATBJMEcGCCsGAQUFBwIBFjto
dHRwOi8vd3d3LnRydXN0ZHN0LmNvbS9jZXJ0aWZpY2F0ZXMvcG9saWN5L0FDRVMt
aW5kZXguaHRtbDAdBgNVHQ4EFgQUCXIGThhDD+XWzMNqizF7eI+og7gwDQYJKoZI
hvcNAQEFBQADggEBAKPYjtay284F5zLNAdMEA+V25FYrnJmQ6AgwbN99Pe7lv7Uk
QIRJ4dEorsTCOlMwiPH1d25Ryvr/ma8kXxug/fKshMrfqfBfBC6tFr8hlxCBPeP/
h40y3JTlR4peahPJlJU90u7INJXQgNStMgiAVDzgvVJT11J8smk/f3rPanTK+gQq
nExaBqXpIK1FZg9p8d2/6eMyi/rgwYZNcjwu2JN4Cir42NInPRmJX1p7ijvMDNpR
rscL9yuwNwXsvFcj4jjSm2jzVhKIT0J8uDHEtdvkyCE06UgRNe76x5JXxZ805Mf2
9w4LTJxoeHtxMcfrHuBnQfO3oKfN5XozNmr6mis=
-----END CERTIFICATE-----

# Issuer: CN=TÜRKTRUST Elektronik Sertifika Hizmet Sağlayıcısı O=(c) 2005 TÜRKTRUST Bilgi İletişim ve Bilişim Güvenliği Hizmetleri A.Ş.
# Subject: CN=TÜRKTRUST Elektronik Sertifika Hizmet Sağlayıcısı O=(c) 2005 TÜRKTRUST Bilgi İletişim ve Bilişim Güvenliği Hizmetleri A.Ş.
# Label: "TURKTRUST Certificate Services Provider Root 1"
# Serial: 1
# MD5 Fingerprint: f1:6a:22:18:c9:cd:df:ce:82:1d:1d:b7:78:5c:a9:a5
# SHA1 Fingerprint: 79:98:a3:08:e1:4d:65:85:e6:c2:1e:15:3a:71:9f:ba:5a:d3:4a:d9
# SHA256 Fingerprint: 44:04:e3:3b:5e:14:0d:cf:99:80:51:fd:fc:80:28:c7:c8:16:15:c5:ee:73:7b:11:1b:58:82:33:a9:b5:35:a0
-----BEGIN CERTIFICATE-----
MIID+zCCAuOgAwIBAgIBATANBgkqhkiG9w0BAQUFADCBtzE/MD0GA1UEAww2VMOc
UktUUlVTVCBFbGVrdHJvbmlrIFNlcnRpZmlrYSBIaXptZXQgU2HEn2xhecSxY8Sx
c8SxMQswCQYDVQQGDAJUUjEPMA0GA1UEBwwGQU5LQVJBMVYwVAYDVQQKDE0oYykg
MjAwNSBUw5xSS1RSVVNUIEJpbGdpIMSwbGV0acWfaW0gdmUgQmlsacWfaW0gR8O8
dmVubGnEn2kgSGl6bWV0bGVyaSBBLsWeLjAeFw0wNTA1MTMxMDI3MTdaFw0xNTAz
MjIxMDI3MTdaMIG3MT8wPQYDVQQDDDZUw5xSS1RSVVNUIEVsZWt0cm9uaWsgU2Vy
dGlmaWthIEhpem1ldCBTYcSfbGF5xLFjxLFzxLExCzAJBgNVBAYMAlRSMQ8wDQYD
VQQHDAZBTktBUkExVjBUBgNVBAoMTShjKSAyMDA1IFTDnFJLVFJVU1QgQmlsZ2kg
xLBsZXRpxZ9pbSB2ZSBCaWxpxZ9pbSBHw7x2ZW5sacSfaSBIaXptZXRsZXJpIEEu
xZ4uMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAylIF1mMD2Bxf3dJ7
XfIMYGFbazt0K3gNfUW9InTojAPBxhEqPZW8qZSwu5GXyGl8hMW0kWxsE2qkVa2k
heiVfrMArwDCBRj1cJ02i67L5BuBf5OI+2pVu32Fks66WJ/bMsW9Xe8iSi9BB35J
YbOG7E6mQW6EvAPs9TscyB/C7qju6hJKjRTP8wrgUDn5CDX4EVmt5yLqS8oUBt5C
urKZ8y1UiBAG6uEaPj1nH/vO+3yC6BFdSsG5FOpU2WabfIl9BJpiyelSPJ6c79L1
JuTm5Rh8i27fbMx4W09ysstcP4wFjdFMjK2Sx+F4f2VsSQZQLJ4ywtdKxnWKWU51
b0dewQIDAQABoxAwDjAMBgNVHRMEBTADAQH/MA0GCSqGSIb3DQEBBQUAA4IBAQAV
9VX/N5aAWSGk/KEVTCD21F/aAyT8z5Aa9CEKmu46sWrv7/hg0Uw2ZkUd82YCdAR7
kjCo3gp2D++Vbr3JN+YaDayJSFvMgzbC9UZcWYJWtNX+I7TYVBxEq8Sn5RTOPEFh
fEPmzcSBCYsk+1Ql1haolgxnB2+zUEfjHCQo3SqYpGH+2+oSN7wBGjSFvW5P55Fy
B0SFHljKVETd96y5y4khctuPwGkplyqjrhgjlxxBKot8KsF8kOipKMDTkcatKIdA
aLX/7KfS0zgYnNN9aV3wxqUeJBujR/xpB2jn5Jq07Q+hh4cCzofSSE7hvP/L8XKS
RGQDJereW26fyfJOrN3H
-----END CERTIFICATE-----

# Issuer: CN=TÜRKTRUST Elektronik Sertifika Hizmet Sağlayıcısı O=TÜRKTRUST Bilgi İletişim ve Bilişim Güvenliği Hizmetleri A.Ş. (c) Kasım 2005
# Subject: CN=TÜRKTRUST Elektronik Sertifika Hizmet Sağlayıcısı O=TÜRKTRUST Bilgi İletişim ve Bilişim Güvenliği Hizmetleri A.Ş. (c) Kasım 2005
# Label: "TURKTRUST Certificate Services Provider Root 2"
# Serial: 1
# MD5 Fingerprint: 37:a5:6e:d4:b1:25:84:97:b7:fd:56:15:7a:f9:a2:00
# SHA1 Fingerprint: b4:35:d4:e1:11:9d:1c:66:90:a7:49:eb:b3:94:bd:63:7b:a7:82:b7
# SHA256 Fingerprint: c4:70:cf:54:7e:23:02:b9:77:fb:29:dd:71:a8:9a:7b:6c:1f:60:77:7b:03:29:f5:60:17:f3:28:bf:4f:6b:e6
-----BEGIN CERTIFICATE-----
MIIEPDCCAySgAwIBAgIBATANBgkqhkiG9w0BAQUFADCBvjE/MD0GA1UEAww2VMOc
UktUUlVTVCBFbGVrdHJvbmlrIFNlcnRpZmlrYSBIaXptZXQgU2HEn2xhecSxY8Sx
c8SxMQswCQYDVQQGEwJUUjEPMA0GA1UEBwwGQW5rYXJhMV0wWwYDVQQKDFRUw5xS
S1RSVVNUIEJpbGdpIMSwbGV0acWfaW0gdmUgQmlsacWfaW0gR8O8dmVubGnEn2kg
SGl6bWV0bGVyaSBBLsWeLiAoYykgS2FzxLFtIDIwMDUwHhcNMDUxMTA3MTAwNzU3
WhcNMTUwOTE2MTAwNzU3WjCBvjE/MD0GA1UEAww2VMOcUktUUlVTVCBFbGVrdHJv
bmlrIFNlcnRpZmlrYSBIaXptZXQgU2HEn2xhecSxY8Sxc8SxMQswCQYDVQQGEwJU
UjEPMA0GA1UEBwwGQW5rYXJhMV0wWwYDVQQKDFRUw5xSS1RSVVNUIEJpbGdpIMSw
bGV0acWfaW0gdmUgQmlsacWfaW0gR8O8dmVubGnEn2kgSGl6bWV0bGVyaSBBLsWe
LiAoYykgS2FzxLFtIDIwMDUwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIB
AQCpNn7DkUNMwxmYCMjHWHtPFoylzkkBH3MOrHUTpvqeLCDe2JAOCtFp0if7qnef
J1Il4std2NiDUBd9irWCPwSOtNXwSadktx4uXyCcUHVPr+G1QRT0mJKIx+XlZEdh
R3n9wFHxwZnn3M5q+6+1ATDcRhzviuyV79z/rxAc653YsKpqhRgNF8k+v/Gb0AmJ
Qv2gQrSdiVFVKc8bcLyEVK3BEx+Y9C52YItdP5qtygy/p1Zbj3e41Z55SZI/4PGX
JHpsmxcPbe9TmJEr5A++WXkHeLuXlfSfadRYhwqp48y2WBmfJiGxxFmNskF1wK1p
zpwACPI2/z7woQ8arBT9pmAPAgMBAAGjQzBBMB0GA1UdDgQWBBTZN7NOBf3Zz58S
Fq62iS/rJTqIHDAPBgNVHQ8BAf8EBQMDBwYAMA8GA1UdEwEB/wQFMAMBAf8wDQYJ
KoZIhvcNAQEFBQADggEBAHJglrfJ3NgpXiOFX7KzLXb7iNcX/nttRbj2hWyfIvwq
ECLsqrkw9qtY1jkQMZkpAL2JZkH7dN6RwRgLn7Vhy506vvWolKMiVW4XSf/SKfE4
Jl3vpao6+XF75tpYHdN0wgH6PmlYX63LaL4ULptswLbcoCb6dxriJNoaN+BnrdFz
gw2lGh1uEpJ+hGIAF728JRhX8tepb1mIvDS3LoV4nZbcFMMsilKbloxSZj2GFotH
uFEJjOp9zYhys2AzsfAKRO8P9Qk3iCQOLGsgOqL6EfJANZxEaGM7rDNvY7wsu/LS
y3Z9fYjYHcgFHW68lKlmjHdxx/qR+i9Rnuk5UrbnBEI=
-----END CERTIFICATE-----

# Issuer: CN=SwissSign Gold CA - G2 O=SwissSign AG
# Subject: CN=SwissSign Gold CA - G2 O=SwissSign AG
# Label: "SwissSign Gold CA - G2"
# Serial: 13492815561806991280
# MD5 Fingerprint: 24:77:d9:a8:91:d1:3b:fa:88:2d:c2:ff:f8:cd:33:93
# SHA1 Fingerprint: d8:c5:38:8a:b7:30:1b:1b:6e:d4:7a:e6:45:25:3a:6f:9f:1a:27:61
# SHA256 Fingerprint: 62:dd:0b:e9:b9:f5:0a:16:3e:a0:f8:e7:5c:05:3b:1e:ca:57:ea:55:c8:68:8f:64:7c:68:81:f2:c8:35:7b:95
-----BEGIN CERTIFICATE-----
MIIFujCCA6KgAwIBAgIJALtAHEP1Xk+wMA0GCSqGSIb3DQEBBQUAMEUxCzAJBgNV
BAYTAkNIMRUwEwYDVQQKEwxTd2lzc1NpZ24gQUcxHzAdBgNVBAMTFlN3aXNzU2ln
biBHb2xkIENBIC0gRzIwHhcNMDYxMDI1MDgzMDM1WhcNMzYxMDI1MDgzMDM1WjBF
MQswCQYDVQQGEwJDSDEVMBMGA1UEChMMU3dpc3NTaWduIEFHMR8wHQYDVQQDExZT
d2lzc1NpZ24gR29sZCBDQSAtIEcyMIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIIC
CgKCAgEAr+TufoskDhJuqVAtFkQ7kpJcyrhdhJJCEyq8ZVeCQD5XJM1QiyUqt2/8
76LQwB8CJEoTlo8jE+YoWACjR8cGp4QjK7u9lit/VcyLwVcfDmJlD909Vopz2q5+
bbqBHH5CjCA12UNNhPqE21Is8w4ndwtrvxEvcnifLtg+5hg3Wipy+dpikJKVyh+c
6bM8K8vzARO/Ws/BtQpgvd21mWRTuKCWs2/iJneRjOBiEAKfNA+k1ZIzUd6+jbqE
emA8atufK+ze3gE/bk3lUIbLtK/tREDFylqM2tIrfKjuvqblCqoOpd8FUrdVxyJd
MmqXl2MT28nbeTZ7hTpKxVKJ+STnnXepgv9VHKVxaSvRAiTysybUa9oEVeXBCsdt
MDeQKuSeFDNeFhdVxVu1yzSJkvGdJo+hB9TGsnhQ2wwMC3wLjEHXuendjIj3o02y
MszYF9rNt85mndT9Xv+9lz4pded+p2JYryU0pUHHPbwNUMoDAw8IWh+Vc3hiv69y
FGkOpeUDDniOJihC8AcLYiAQZzlG+qkDzAQ4embvIIO1jEpWjpEA/I5cgt6IoMPi
aG59je883WX0XaxR7ySArqpWl2/5rX3aYT+YdzylkbYcjCbaZaIJbcHiVOO5ykxM
gI93e2CaHt+28kgeDrpOVG2Y4OGiGqJ3UM/EY5LsRxmd6+ZrzsECAwEAAaOBrDCB
qTAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQUWyV7
lqRlUX64OfPAeGZe6Drn8O4wHwYDVR0jBBgwFoAUWyV7lqRlUX64OfPAeGZe6Drn
8O4wRgYDVR0gBD8wPTA7BglghXQBWQECAQEwLjAsBggrBgEFBQcCARYgaHR0cDov
L3JlcG9zaXRvcnkuc3dpc3NzaWduLmNvbS8wDQYJKoZIhvcNAQEFBQADggIBACe6
45R88a7A3hfm5djV9VSwg/S7zV4Fe0+fdWavPOhWfvxyeDgD2StiGwC5+OlgzczO
UYrHUDFu4Up+GC9pWbY9ZIEr44OE5iKHjn3g7gKZYbge9LgriBIWhMIxkziWMaa5
O1M/wySTVltpkuzFwbs4AOPsF6m43Md8AYOfMke6UiI0HTJ6CVanfCU2qT1L2sCC
bwq7EsiHSycR+R4tx5M/nttfJmtS2S6K8RTGRI0Vqbe/vd6mGu6uLftIdxf+u+yv
GPUqUfA5hJeVbG4bwyvEdGB5JbAKJ9/fXtI5z0V9QkvfsywexcZdylU6oJxpmo/a
77KwPJ+HbBIrZXAVUjEaJM9vMSNQH4xPjyPDdEFjHFWoFN0+4FFQz/EbMFYOkrCC
hdiDyyJkvC24JdVUorgG6q2SpCSgwYa1ShNqR88uC1aVVMvOmttqtKay20EIhid3
92qgQmwLOM7XdVAyksLfKzAiSNDVQTglXaTpXZ/GlHXQRf0wl0OPkKsKx4ZzYEpp
Ld6leNcG2mqeSz53OiATIgHQv2ieY2BrNU0LbbqhPcCT4H8js1WtciVORvnSFu+w
ZMEBnunKoGqYDs/YYPIvSbjkQuE4NRb0yG5P94FW6LqjviOvrv1vA+ACOzB2+htt
Qc8Bsem4yWb02ybzOqR08kkkW8mw0FfB+j564ZfJ
-----END CERTIFICATE-----

# Issuer: CN=SwissSign Silver CA - G2 O=SwissSign AG
# Subject: CN=SwissSign Silver CA - G2 O=SwissSign AG
# Label: "SwissSign Silver CA - G2"
# Serial: 5700383053117599563
# MD5 Fingerprint: e0:06:a1:c9:7d:cf:c9:fc:0d:c0:56:75:96:d8:62:13
# SHA1 Fingerprint: 9b:aa:e5:9f:56:ee:21:cb:43:5a:be:25:93:df:a7:f0:40:d1:1d:cb
# SHA256 Fingerprint: be:6c:4d:a2:bb:b9:ba:59:b6:f3:93:97:68:37:42:46:c3:c0:05:99:3f:a9:8f:02:0d:1d:ed:be:d4:8a:81:d5
-----BEGIN CERTIFICATE-----
MIIFvTCCA6WgAwIBAgIITxvUL1S7L0swDQYJKoZIhvcNAQEFBQAwRzELMAkGA1UE
BhMCQ0gxFTATBgNVBAoTDFN3aXNzU2lnbiBBRzEhMB8GA1UEAxMYU3dpc3NTaWdu
IFNpbHZlciBDQSAtIEcyMB4XDTA2MTAyNTA4MzI0NloXDTM2MTAyNTA4MzI0Nlow
RzELMAkGA1UEBhMCQ0gxFTATBgNVBAoTDFN3aXNzU2lnbiBBRzEhMB8GA1UEAxMY
U3dpc3NTaWduIFNpbHZlciBDQSAtIEcyMIICIjANBgkqhkiG9w0BAQEFAAOCAg8A
MIICCgKCAgEAxPGHf9N4Mfc4yfjDmUO8x/e8N+dOcbpLj6VzHVxumK4DV644N0Mv
Fz0fyM5oEMF4rhkDKxD6LHmD9ui5aLlV8gREpzn5/ASLHvGiTSf5YXu6t+WiE7br
YT7QbNHm+/pe7R20nqA1W6GSy/BJkv6FCgU+5tkL4k+73JU3/JHpMjUi0R86TieF
nbAVlDLaYQ1HTWBCrpJH6INaUFjpiou5XaHc3ZlKHzZnu0jkg7Y360g6rw9njxcH
6ATK72oxh9TAtvmUcXtnZLi2kUpCe2UuMGoM9ZDulebyzYLs2aFK7PayS+VFheZt
eJMELpyCbTapxDFkH4aDCyr0NQp4yVXPQbBH6TCfmb5hqAaEuSh6XzjZG6k4sIN/
c8HDO0gqgg8hm7jMqDXDhBuDsz6+pJVpATqJAHgE2cn0mRmrVn5bi4Y5FZGkECwJ
MoBgs5PAKrYYC51+jUnyEEp/+dVGLxmSo5mnJqy7jDzmDrxHB9xzUfFwZC8I+bRH
HTBsROopN4WSaGa8gzj+ezku01DwH/teYLappvonQfGbGHLy9YR0SslnxFSuSGTf
jNFusB3hB48IHpmccelM2KX3RxIfdNFRnobzwqIjQAtz20um53MGjMGg6cFZrEb6
5i/4z3GcRm25xBWNOHkDRUjvxF3XCO6HOSKGsg0PWEP3calILv3q1h8CAwEAAaOB
rDCBqTAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQU
F6DNweRBtjpbO8tFnb0cwpj6hlgwHwYDVR0jBBgwFoAUF6DNweRBtjpbO8tFnb0c
wpj6hlgwRgYDVR0gBD8wPTA7BglghXQBWQEDAQEwLjAsBggrBgEFBQcCARYgaHR0
cDovL3JlcG9zaXRvcnkuc3dpc3NzaWduLmNvbS8wDQYJKoZIhvcNAQEFBQADggIB
AHPGgeAn0i0P4JUw4ppBf1AsX19iYamGamkYDHRJ1l2E6kFSGG9YrVBWIGrGvShp
WJHckRE1qTodvBqlYJ7YH39FkWnZfrt4csEGDyrOj4VwYaygzQu4OSlWhDJOhrs9
xCrZ1x9y7v5RoSJBsXECYxqCsGKrXlcSH9/L3XWgwF15kIwb4FDm3jH+mHtwX6WQ
2K34ArZv02DdQEsixT2tOnqfGhpHkXkzuoLcMmkDlm4fS/Bx/uNncqCxv1yL5PqZ
IseEuRuNI5c/7SXgz2W79WEE790eslpBIlqhn10s6FvJbakMDHiqYMZWjwFaDGi8
aRl5xB9+lwW/xekkUV7U1UtT7dkjWjYDZaPBA61BMPNGG4WQr2W11bHkFlt4dR2X
em1ZqSqPe97Dh4kQmUlzeMg9vVE1dCrV8X5pGyq7O70luJpaPXJhkGaH7gzWTdQR
dAtq/gsD/KNVV4n+SsuuWxcFyPKNIzFTONItaj+CuY0IavdeQXRuwxF+B6wpYJE/
OMpXEA29MC/HpeZBoNquBYeaoKRlbEwJDIm6uNO5wJOKMPqN5ZprFQFOZ6raYlY+
hAhm0sQ2fac+EPyI4NSA5QC9qvNOBqN6avlicuMJT+ubDgEj8Z+7fNzcbBGXJbLy
tGMU0gYqZ4yD9c7qB9iaah7s5Aq7KkzrCWA5zspi2C5u
-----END CERTIFICATE-----

# Issuer: CN=GeoTrust Primary Certification Authority O=GeoTrust Inc.
# Subject: CN=GeoTrust Primary Certification Authority O=GeoTrust Inc.
# Label: "GeoTrust Primary Certification Authority"
# Serial: 32798226551256963324313806436981982369
# MD5 Fingerprint: 02:26:c3:01:5e:08:30:37:43:a9:d0:7d:cf:37:e6:bf
# SHA1 Fingerprint: 32:3c:11:8e:1b:f7:b8:b6:52:54:e2:e2:10:0d:d6:02:90:37:f0:96
# SHA256 Fingerprint: 37:d5:10:06:c5:12:ea:ab:62:64:21:f1:ec:8c:92:01:3f:c5:f8:2a:e9:8e:e5:33:eb:46:19:b8:de:b4:d0:6c
-----BEGIN CERTIFICATE-----
MIIDfDCCAmSgAwIBAgIQGKy1av1pthU6Y2yv2vrEoTANBgkqhkiG9w0BAQUFADBY
MQswCQYDVQQGEwJVUzEWMBQGA1UEChMNR2VvVHJ1c3QgSW5jLjExMC8GA1UEAxMo
R2VvVHJ1c3QgUHJpbWFyeSBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTAeFw0wNjEx
MjcwMDAwMDBaFw0zNjA3MTYyMzU5NTlaMFgxCzAJBgNVBAYTAlVTMRYwFAYDVQQK
Ew1HZW9UcnVzdCBJbmMuMTEwLwYDVQQDEyhHZW9UcnVzdCBQcmltYXJ5IENlcnRp
ZmljYXRpb24gQXV0aG9yaXR5MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKC
AQEAvrgVe//UfH1nrYNke8hCUy3f9oQIIGHWAVlqnEQRr+92/ZV+zmEwu3qDXwK9
AWbK7hWNb6EwnL2hhZ6UOvNWiAAxz9juapYC2e0DjPt1befquFUWBRaa9OBesYjA
ZIVcFU2Ix7e64HXprQU9nceJSOC7KMgD4TCTZF5SwFlwIjVXiIrxlQqD17wxcwE0
7e9GceBrAqg1cmuXm2bgyxx5X9gaBGgeRwLmnWDiNpcB3841kt++Z8dtd1k7j53W
kBWUvEI0EME5+bEnPn7WinXFsq+W06Lem+SYvn3h6YGttm/81w7a4DSwDRp35+MI
mO9Y+pyEtzavwt+s0vQQBnBxNQIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4G
A1UdDwEB/wQEAwIBBjAdBgNVHQ4EFgQULNVQQZcVi/CPNmFbSvtr2ZnJM5IwDQYJ
KoZIhvcNAQEFBQADggEBAFpwfyzdtzRP9YZRqSa+S7iq8XEN3GHHoOo0Hnp3DwQ1
6CePbJC/kRYkRj5KTs4rFtULUh38H2eiAkUxT87z+gOneZ1TatnaYzr4gNfTmeGl
4b7UVXGYNTq+k+qurUKykG/g/CFNNWMziUnWm07Kx+dOCQD32sfvmWKZd7aVIl6K
oKv0uHiYyjgZmclynnjNS6yvGaBzEi38wkG6gZHaFloxt/m0cYASSJlyc1pZU8Fj
UjPtp8nSOQJw+uCxQmYpqptR7TBUIhRf2asdweSU8Pj1K/fqynhG1riR/aYNKxoU
AT6A8EKglQdebc3MS6RFjasS6LPeWuWgfOgPIh1a6Vk=
-----END CERTIFICATE-----

# Issuer: CN=thawte Primary Root CA O=thawte, Inc. OU=Certification Services Division/(c) 2006 thawte, Inc. - For authorized use only
# Subject: CN=thawte Primary Root CA O=thawte, Inc. OU=Certification Services Division/(c) 2006 thawte, Inc. - For authorized use only
# Label: "thawte Primary Root CA"
# Serial: 69529181992039203566298953787712940909
# MD5 Fingerprint: 8c:ca:dc:0b:22:ce:f5:be:72:ac:41:1a:11:a8:d8:12
# SHA1 Fingerprint: 91:c6:d6:ee:3e:8a:c8:63:84:e5:48:c2:99:29:5c:75:6c:81:7b:81
# SHA256 Fingerprint: 8d:72:2f:81:a9:c1:13:c0:79:1d:f1:36:a2:96:6d:b2:6c:95:0a:97:1d:b4:6b:41:99:f4:ea:54:b7:8b:fb:9f
-----BEGIN CERTIFICATE-----
MIIEIDCCAwigAwIBAgIQNE7VVyDV7exJ9C/ON9srbTANBgkqhkiG9w0BAQUFADCB
qTELMAkGA1UEBhMCVVMxFTATBgNVBAoTDHRoYXd0ZSwgSW5jLjEoMCYGA1UECxMf
Q2VydGlmaWNhdGlvbiBTZXJ2aWNlcyBEaXZpc2lvbjE4MDYGA1UECxMvKGMpIDIw
MDYgdGhhd3RlLCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxHzAdBgNV
BAMTFnRoYXd0ZSBQcmltYXJ5IFJvb3QgQ0EwHhcNMDYxMTE3MDAwMDAwWhcNMzYw
NzE2MjM1OTU5WjCBqTELMAkGA1UEBhMCVVMxFTATBgNVBAoTDHRoYXd0ZSwgSW5j
LjEoMCYGA1UECxMfQ2VydGlmaWNhdGlvbiBTZXJ2aWNlcyBEaXZpc2lvbjE4MDYG
A1UECxMvKGMpIDIwMDYgdGhhd3RlLCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNl
IG9ubHkxHzAdBgNVBAMTFnRoYXd0ZSBQcmltYXJ5IFJvb3QgQ0EwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQCsoPD7gFnUnMekz52hWXMJEEUMDSxuaPFs
W0hoSVk3/AszGcJ3f8wQLZU0HObrTQmnHNK4yZc2AreJ1CRfBsDMRJSUjQJib+ta
3RGNKJpchJAQeg29dGYvajig4tVUROsdB58Hum/u6f1OCyn1PoSgAfGcq/gcfomk
6KHYcWUNo1F77rzSImANuVud37r8UVsLr5iy6S7pBOhih94ryNdOwUxkHt3Ph1i6
Sk/KaAcdHJ1KxtUvkcx8cXIcxcBn6zL9yZJclNqFwJu/U30rCfSMnZEfl2pSy94J
NqR32HuHUETVPm4pafs5SSYeCaWAe0At6+gnhcn+Yf1+5nyXHdWdAgMBAAGjQjBA
MA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMB0GA1UdDgQWBBR7W0XP
r87Lev0xkhpqtvNG61dIUDANBgkqhkiG9w0BAQUFAAOCAQEAeRHAS7ORtvzw6WfU
DW5FvlXok9LOAz/t2iWwHVfLHjp2oEzsUHboZHIMpKnxuIvW1oeEuzLlQRHAd9mz
YJ3rG9XRbkREqaYB7FViHXe4XI5ISXycO1cRrK1zN44veFyQaEfZYGDm/Ac9IiAX
xPcW6cTYcvnIc3zfFi8VqT79aie2oetaupgf1eNNZAqdE8hhuvU5HIe6uL17In/2
/qxAeeWsEG89jxt5dovEN7MhGITlNgDrYyCZuen+MwS7QcjBAvlEYyCegc5C09Y/
LHbTY5xZ3Y+m4Q6gLkH3LpVHz7z9M/P2C2F+fpErgUfCJzDupxBdN49cOSvkBPB7
jVaMaA==
-----END CERTIFICATE-----

# Issuer: CN=VeriSign Class 3 Public Primary Certification Authority - G5 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 2006 VeriSign, Inc. - For authorized use only
# Subject: CN=VeriSign Class 3 Public Primary Certification Authority - G5 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 2006 VeriSign, Inc. - For authorized use only
# Label: "VeriSign Class 3 Public Primary Certification Authority - G5"
# Serial: 33037644167568058970164719475676101450
# MD5 Fingerprint: cb:17:e4:31:67:3e:e2:09:fe:45:57:93:f3:0a:fa:1c
# SHA1 Fingerprint: 4e:b6:d5:78:49:9b:1c:cf:5f:58:1e:ad:56:be:3d:9b:67:44:a5:e5
# SHA256 Fingerprint: 9a:cf:ab:7e:43:c8:d8:80:d0:6b:26:2a:94:de:ee:e4:b4:65:99:89:c3:d0:ca:f1:9b:af:64:05:e4:1a:b7:df
-----BEGIN CERTIFICATE-----
MIIE0zCCA7ugAwIBAgIQGNrRniZ96LtKIVjNzGs7SjANBgkqhkiG9w0BAQUFADCB
yjELMAkGA1UEBhMCVVMxFzAVBgNVBAoTDlZlcmlTaWduLCBJbmMuMR8wHQYDVQQL
ExZWZXJpU2lnbiBUcnVzdCBOZXR3b3JrMTowOAYDVQQLEzEoYykgMjAwNiBWZXJp
U2lnbiwgSW5jLiAtIEZvciBhdXRob3JpemVkIHVzZSBvbmx5MUUwQwYDVQQDEzxW
ZXJpU2lnbiBDbGFzcyAzIFB1YmxpYyBQcmltYXJ5IENlcnRpZmljYXRpb24gQXV0
aG9yaXR5IC0gRzUwHhcNMDYxMTA4MDAwMDAwWhcNMzYwNzE2MjM1OTU5WjCByjEL
MAkGA1UEBhMCVVMxFzAVBgNVBAoTDlZlcmlTaWduLCBJbmMuMR8wHQYDVQQLExZW
ZXJpU2lnbiBUcnVzdCBOZXR3b3JrMTowOAYDVQQLEzEoYykgMjAwNiBWZXJpU2ln
biwgSW5jLiAtIEZvciBhdXRob3JpemVkIHVzZSBvbmx5MUUwQwYDVQQDEzxWZXJp
U2lnbiBDbGFzcyAzIFB1YmxpYyBQcmltYXJ5IENlcnRpZmljYXRpb24gQXV0aG9y
aXR5IC0gRzUwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCvJAgIKXo1
nmAMqudLO07cfLw8RRy7K+D+KQL5VwijZIUVJ/XxrcgxiV0i6CqqpkKzj/i5Vbex
t0uz/o9+B1fs70PbZmIVYc9gDaTY3vjgw2IIPVQT60nKWVSFJuUrjxuf6/WhkcIz
SdhDY2pSS9KP6HBRTdGJaXvHcPaz3BJ023tdS1bTlr8Vd6Gw9KIl8q8ckmcY5fQG
BO+QueQA5N06tRn/Arr0PO7gi+s3i+z016zy9vA9r911kTMZHRxAy3QkGSGT2RT+
rCpSx4/VBEnkjWNHiDxpg8v+R70rfk/Fla4OndTRQ8Bnc+MUCH7lP59zuDMKz10/
NIeWiu5T6CUVAgMBAAGjgbIwga8wDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8E
BAMCAQYwbQYIKwYBBQUHAQwEYTBfoV2gWzBZMFcwVRYJaW1hZ2UvZ2lmMCEwHzAH
BgUrDgMCGgQUj+XTGoasjY5rw8+AatRIGCx7GS4wJRYjaHR0cDovL2xvZ28udmVy
aXNpZ24uY29tL3ZzbG9nby5naWYwHQYDVR0OBBYEFH/TZafC3ey78DAJ80M5+gKv
MzEzMA0GCSqGSIb3DQEBBQUAA4IBAQCTJEowX2LP2BqYLz3q3JktvXf2pXkiOOzE
p6B4Eq1iDkVwZMXnl2YtmAl+X6/WzChl8gGqCBpH3vn5fJJaCGkgDdk+bW48DW7Y
5gaRQBi5+MHt39tBquCWIMnNZBU4gcmU7qKEKQsTb47bDN0lAtukixlE0kF6BWlK
WE9gyn6CagsCqiUXObXbf+eEZSqVir2G3l6BFoMtEMze/aiCKm0oHw0LxOXnGiYZ
4fQRbxC1lfznQgUy286dUV4otp6F01vvpX1FQHKOtw5rDgb7MzVIcbidJ4vEZV8N
hnacRHr2lVz2XTIIM6RUthg/aFzyQkqFOFSDX9HoLPKsEdao7WNq
-----END CERTIFICATE-----

# Issuer: CN=SecureTrust CA O=SecureTrust Corporation
# Subject: CN=SecureTrust CA O=SecureTrust Corporation
# Label: "SecureTrust CA"
# Serial: 17199774589125277788362757014266862032
# MD5 Fingerprint: dc:32:c3:a7:6d:25:57:c7:68:09:9d:ea:2d:a9:a2:d1
# SHA1 Fingerprint: 87:82:c6:c3:04:35:3b:cf:d2:96:92:d2:59:3e:7d:44:d9:34:ff:11
# SHA256 Fingerprint: f1:c1:b5:0a:e5:a2:0d:d8:03:0e:c9:f6:bc:24:82:3d:d3:67:b5:25:57:59:b4:e7:1b:61:fc:e9:f7:37:5d:73
-----BEGIN CERTIFICATE-----
MIIDuDCCAqCgAwIBAgIQDPCOXAgWpa1Cf/DrJxhZ0DANBgkqhkiG9w0BAQUFADBI
MQswCQYDVQQGEwJVUzEgMB4GA1UEChMXU2VjdXJlVHJ1c3QgQ29ycG9yYXRpb24x
FzAVBgNVBAMTDlNlY3VyZVRydXN0IENBMB4XDTA2MTEwNzE5MzExOFoXDTI5MTIz
MTE5NDA1NVowSDELMAkGA1UEBhMCVVMxIDAeBgNVBAoTF1NlY3VyZVRydXN0IENv
cnBvcmF0aW9uMRcwFQYDVQQDEw5TZWN1cmVUcnVzdCBDQTCCASIwDQYJKoZIhvcN
AQEBBQADggEPADCCAQoCggEBAKukgeWVzfX2FI7CT8rU4niVWJxB4Q2ZQCQXOZEz
Zum+4YOvYlyJ0fwkW2Gz4BERQRwdbvC4u/jep4G6pkjGnx29vo6pQT64lO0pGtSO
0gMdA+9tDWccV9cGrcrI9f4Or2YlSASWC12juhbDCE/RRvgUXPLIXgGZbf2IzIao
wW8xQmxSPmjL8xk037uHGFaAJsTQ3MBv396gwpEWoGQRS0S8Hvbn+mPeZqx2pHGj
7DaUaHp3pLHnDi+BeuK1cobvomuL8A/b01k/unK8RCSc43Oz969XL0Imnal0ugBS
8kvNU3xHCzaFDmapCJcWNFfBZveA4+1wVMeT4C4oFVmHursCAwEAAaOBnTCBmjAT
BgkrBgEEAYI3FAIEBh4EAEMAQTALBgNVHQ8EBAMCAYYwDwYDVR0TAQH/BAUwAwEB
/zAdBgNVHQ4EFgQUQjK2FvoE/f5dS3rD/fdMQB1aQ68wNAYDVR0fBC0wKzApoCeg
JYYjaHR0cDovL2NybC5zZWN1cmV0cnVzdC5jb20vU1RDQS5jcmwwEAYJKwYBBAGC
NxUBBAMCAQAwDQYJKoZIhvcNAQEFBQADggEBADDtT0rhWDpSclu1pqNlGKa7UTt3
6Z3q059c4EVlew3KW+JwULKUBRSuSceNQQcSc5R+DCMh/bwQf2AQWnL1mA6s7Ll/
3XpvXdMc9P+IBWlCqQVxyLesJugutIxq/3HcuLHfmbx8IVQr5Fiiu1cprp6poxkm
D5kuCLDv/WnPmRoJjeOnnyvJNjR7JLN4TJUXpAYmHrZkUjZfYGfZnMUFdAvnZyPS
CPyI6a6Lf+Ew9Dd+/cYy2i2eRDAwbO4H3tI0/NL/QPZL9GZGBlSm8jIKYyYwa5vR
3ItHuuG51WLQoqD0ZwV4KWMabwTW+MZMo5qxN7SN5ShLHZ4swrhovO0C7jE=
-----END CERTIFICATE-----

# Issuer: CN=Secure Global CA O=SecureTrust Corporation
# Subject: CN=Secure Global CA O=SecureTrust Corporation
# Label: "Secure Global CA"
# Serial: 9751836167731051554232119481456978597
# MD5 Fingerprint: cf:f4:27:0d:d4:ed:dc:65:16:49:6d:3d:da:bf:6e:de
# SHA1 Fingerprint: 3a:44:73:5a:e5:81:90:1f:24:86:61:46:1e:3b:9c:c4:5f:f5:3a:1b
# SHA256 Fingerprint: 42:00:f5:04:3a:c8:59:0e:bb:52:7d:20:9e:d1:50:30:29:fb:cb:d4:1c:a1:b5:06:ec:27:f1:5a:de:7d:ac:69
-----BEGIN CERTIFICATE-----
MIIDvDCCAqSgAwIBAgIQB1YipOjUiolN9BPI8PjqpTANBgkqhkiG9w0BAQUFADBK
MQswCQYDVQQGEwJVUzEgMB4GA1UEChMXU2VjdXJlVHJ1c3QgQ29ycG9yYXRpb24x
GTAXBgNVBAMTEFNlY3VyZSBHbG9iYWwgQ0EwHhcNMDYxMTA3MTk0MjI4WhcNMjkx
MjMxMTk1MjA2WjBKMQswCQYDVQQGEwJVUzEgMB4GA1UEChMXU2VjdXJlVHJ1c3Qg
Q29ycG9yYXRpb24xGTAXBgNVBAMTEFNlY3VyZSBHbG9iYWwgQ0EwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQCvNS7YrGxVaQZx5RNoJLNP2MwhR/jxYDiJ
iQPpvepeRlMJ3Fz1Wuj3RSoC6zFh1ykzTM7HfAo3fg+6MpjhHZevj8fcyTiW89sa
/FHtaMbQbqR8JNGuQsiWUGMu4P51/pinX0kuleM5M2SOHqRfkNJnPLLZ/kG5VacJ
jnIFHovdRIWCQtBJwB1g8NEXLJXr9qXBkqPFwqcIYA1gBBCWeZ4WNOaptvolRTnI
HmX5k/Wq8VLcmZg9pYYaDDUz+kulBAYVHDGA76oYa8J719rO+TMg1fW9ajMtgQT7
sFzUnKPiXB3jqUJ1XnvUd+85VLrJChgbEplJL4hL/VBi0XPnj3pDAgMBAAGjgZ0w
gZowEwYJKwYBBAGCNxQCBAYeBABDAEEwCwYDVR0PBAQDAgGGMA8GA1UdEwEB/wQF
MAMBAf8wHQYDVR0OBBYEFK9EBMJBfkiD2045AuzshHrmzsmkMDQGA1UdHwQtMCsw
KaAnoCWGI2h0dHA6Ly9jcmwuc2VjdXJldHJ1c3QuY29tL1NHQ0EuY3JsMBAGCSsG
AQQBgjcVAQQDAgEAMA0GCSqGSIb3DQEBBQUAA4IBAQBjGghAfaReUw132HquHw0L
URYD7xh8yOOvaliTFGCRsoTciE6+OYo68+aCiV0BN7OrJKQVDpI1WkpEXk5X+nXO
H0jOZvQ8QCaSmGwb7iRGDBezUqXbpZGRzzfTb+cnCDpOGR86p1hcF895P4vkp9Mm
I50mD1hp/Ed+stCNi5O/KU9DaXR2Z0vPB4zmAve14bRDtUstFJ/53CYNv6ZHdAbY
iNE6KTCEztI5gGIbqMdXSbxqVVFnFUq+NQfk1XWYN3kwFNspnWzFacxHVaIw98xc
f8LDmBxrThaA63p4ZUWiABqvDA1VZDRIuJK58bRQKfJPIx/abKwfROHdI3hRW8cW
-----END CERTIFICATE-----

# Issuer: CN=COMODO Certification Authority O=COMODO CA Limited
# Subject: CN=COMODO Certification Authority O=COMODO CA Limited
# Label: "COMODO Certification Authority"
# Serial: 104350513648249232941998508985834464573
# MD5 Fingerprint: 5c:48:dc:f7:42:72:ec:56:94:6d:1c:cc:71:35:80:75
# SHA1 Fingerprint: 66:31:bf:9e:f7:4f:9e:b6:c9:d5:a6:0c:ba:6a:be:d1:f7:bd:ef:7b
# SHA256 Fingerprint: 0c:2c:d6:3d:f7:80:6f:a3:99:ed:e8:09:11:6b:57:5b:f8:79:89:f0:65:18:f9:80:8c:86:05:03:17:8b:af:66
-----BEGIN CERTIFICATE-----
MIIEHTCCAwWgAwIBAgIQToEtioJl4AsC7j41AkblPTANBgkqhkiG9w0BAQUFADCB
gTELMAkGA1UEBhMCR0IxGzAZBgNVBAgTEkdyZWF0ZXIgTWFuY2hlc3RlcjEQMA4G
A1UEBxMHU2FsZm9yZDEaMBgGA1UEChMRQ09NT0RPIENBIExpbWl0ZWQxJzAlBgNV
BAMTHkNPTU9ETyBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTAeFw0wNjEyMDEwMDAw
MDBaFw0yOTEyMzEyMzU5NTlaMIGBMQswCQYDVQQGEwJHQjEbMBkGA1UECBMSR3Jl
YXRlciBNYW5jaGVzdGVyMRAwDgYDVQQHEwdTYWxmb3JkMRowGAYDVQQKExFDT01P
RE8gQ0EgTGltaXRlZDEnMCUGA1UEAxMeQ09NT0RPIENlcnRpZmljYXRpb24gQXV0
aG9yaXR5MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0ECLi3LjkRv3
UcEbVASY06m/weaKXTuH+7uIzg3jLz8GlvCiKVCZrts7oVewdFFxze1CkU1B/qnI
2GqGd0S7WWaXUF601CxwRM/aN5VCaTwwxHGzUvAhTaHYujl8HJ6jJJ3ygxaYqhZ8
Q5sVW7euNJH+1GImGEaaP+vB+fGQV+useg2L23IwambV4EajcNxo2f8ESIl33rXp
+2dtQem8Ob0y2WIC8bGoPW43nOIv4tOiJovGuFVDiOEjPqXSJDlqR6sA1KGzqSX+
DT+nHbrTUcELpNqsOO9VUCQFZUaTNE8tja3G1CEZ0o7KBWFxB3NH5YoZEr0ETc5O
nKVIrLsm9wIDAQABo4GOMIGLMB0GA1UdDgQWBBQLWOWLxkwVN6RAqTCpIb5HNlpW
/zAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB/zBJBgNVHR8EQjBAMD6g
PKA6hjhodHRwOi8vY3JsLmNvbW9kb2NhLmNvbS9DT01PRE9DZXJ0aWZpY2F0aW9u
QXV0aG9yaXR5LmNybDANBgkqhkiG9w0BAQUFAAOCAQEAPpiem/Yb6dc5t3iuHXIY
SdOH5EOC6z/JqvWote9VfCFSZfnVDeFs9D6Mk3ORLgLETgdxb8CPOGEIqB6BCsAv
IC9Bi5HcSEW88cbeunZrM8gALTFGTO3nnc+IlP8zwFboJIYmuNg4ON8qa90SzMc/
RxdMosIGlgnW2/4/PEZB31jiVg88O8EckzXZOFKs7sjsLjBOlDW0JB9LeGna8gI4
zJVSk/BwJVmcIGfE7vmLV2H0knZ9P4SNVbfo5azV8fUZVqZa+5Acr5Pr5RzUZ5dd
BA6+C4OmF4O5MBKgxTMVBbkN+8cFduPYSo38NBejxiEovjBFMR7HeL5YYTisO+IB
ZQ==
-----END CERTIFICATE-----

# Issuer: CN=Network Solutions Certificate Authority O=Network Solutions L.L.C.
# Subject: CN=Network Solutions Certificate Authority O=Network Solutions L.L.C.
# Label: "Network Solutions Certificate Authority"
# Serial: 116697915152937497490437556386812487904
# MD5 Fingerprint: d3:f3:a6:16:c0:fa:6b:1d:59:b1:2d:96:4d:0e:11:2e
# SHA1 Fingerprint: 74:f8:a3:c3:ef:e7:b3:90:06:4b:83:90:3c:21:64:60:20:e5:df:ce
# SHA256 Fingerprint: 15:f0:ba:00:a3:ac:7a:f3:ac:88:4c:07:2b:10:11:a0:77:bd:77:c0:97:f4:01:64:b2:f8:59:8a:bd:83:86:0c
-----BEGIN CERTIFICATE-----
MIID5jCCAs6gAwIBAgIQV8szb8JcFuZHFhfjkDFo4DANBgkqhkiG9w0BAQUFADBi
MQswCQYDVQQGEwJVUzEhMB8GA1UEChMYTmV0d29yayBTb2x1dGlvbnMgTC5MLkMu
MTAwLgYDVQQDEydOZXR3b3JrIFNvbHV0aW9ucyBDZXJ0aWZpY2F0ZSBBdXRob3Jp
dHkwHhcNMDYxMjAxMDAwMDAwWhcNMjkxMjMxMjM1OTU5WjBiMQswCQYDVQQGEwJV
UzEhMB8GA1UEChMYTmV0d29yayBTb2x1dGlvbnMgTC5MLkMuMTAwLgYDVQQDEydO
ZXR3b3JrIFNvbHV0aW9ucyBDZXJ0aWZpY2F0ZSBBdXRob3JpdHkwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQDkvH6SMG3G2I4rC7xGzuAnlt7e+foS0zwz
c7MEL7xxjOWftiJgPl9dzgn/ggwbmlFQGiaJ3dVhXRncEg8tCqJDXRfQNJIg6nPP
OCwGJgl6cvf6UDL4wpPTaaIjzkGxzOTVHzbRijr4jGPiFFlp7Q3Tf2vouAPlT2rl
mGNpSAW+Lv8ztumXWWn4Zxmuk2GWRBXTcrA/vGp97Eh/jcOrqnErU2lBUzS1sLnF
BgrEsEX1QV1uiUV7PTsmjHTC5dLRfbIR1PtYMiKagMnc/Qzpf14Dl847ABSHJ3A4
qY5usyd2mFHgBeMhqxrVhSI8KbWaFsWAqPS7azCPL0YCorEMIuDTAgMBAAGjgZcw
gZQwHQYDVR0OBBYEFCEwyfsA106Y2oeqKtCnLrFAMadMMA4GA1UdDwEB/wQEAwIB
BjAPBgNVHRMBAf8EBTADAQH/MFIGA1UdHwRLMEkwR6BFoEOGQWh0dHA6Ly9jcmwu
bmV0c29sc3NsLmNvbS9OZXR3b3JrU29sdXRpb25zQ2VydGlmaWNhdGVBdXRob3Jp
dHkuY3JsMA0GCSqGSIb3DQEBBQUAA4IBAQC7rkvnt1frf6ott3NHhWrB5KUd5Oc8
6fRZZXe1eltajSU24HqXLjjAV2CDmAaDn7l2em5Q4LqILPxFzBiwmZVRDuwduIj/
h1AcgsLj4DKAv6ALR8jDMe+ZZzKATxcheQxpXN5eNK4CtSbqUN9/GGUsyfJj4akH
/nxxH2szJGoeBfcFaMBqEssuXmHLrijTfsK0ZpEmXzwuJF/LWA/rKOyvEZbz3Htv
wKeI8lN3s2Berq4o2jUsbzRF0ybh3uxbTydrFny9RAQYgrOJeRcQcT16ohZO9QHN
pGxlaKFJdlxDydi8NmdspZS11My5vWo1ViHe2MPr+8ukYEywVaCge1ey
-----END CERTIFICATE-----

# Issuer: CN=WellsSecure Public Root Certificate Authority O=Wells Fargo WellsSecure OU=Wells Fargo Bank NA
# Subject: CN=WellsSecure Public Root Certificate Authority O=Wells Fargo WellsSecure OU=Wells Fargo Bank NA
# Label: "WellsSecure Public Root Certificate Authority"
# Serial: 1
# MD5 Fingerprint: 15:ac:a5:c2:92:2d:79:bc:e8:7f:cb:67:ed:02:cf:36
# SHA1 Fingerprint: e7:b4:f6:9d:61:ec:90:69:db:7e:90:a7:40:1a:3c:f4:7d:4f:e8:ee
# SHA256 Fingerprint: a7:12:72:ae:aa:a3:cf:e8:72:7f:7f:b3:9f:0f:b3:d1:e5:42:6e:90:60:b0:6e:e6:f1:3e:9a:3c:58:33:cd:43
-----BEGIN CERTIFICATE-----
MIIEvTCCA6WgAwIBAgIBATANBgkqhkiG9w0BAQUFADCBhTELMAkGA1UEBhMCVVMx
IDAeBgNVBAoMF1dlbGxzIEZhcmdvIFdlbGxzU2VjdXJlMRwwGgYDVQQLDBNXZWxs
cyBGYXJnbyBCYW5rIE5BMTYwNAYDVQQDDC1XZWxsc1NlY3VyZSBQdWJsaWMgUm9v
dCBDZXJ0aWZpY2F0ZSBBdXRob3JpdHkwHhcNMDcxMjEzMTcwNzU0WhcNMjIxMjE0
MDAwNzU0WjCBhTELMAkGA1UEBhMCVVMxIDAeBgNVBAoMF1dlbGxzIEZhcmdvIFdl
bGxzU2VjdXJlMRwwGgYDVQQLDBNXZWxscyBGYXJnbyBCYW5rIE5BMTYwNAYDVQQD
DC1XZWxsc1NlY3VyZSBQdWJsaWMgUm9vdCBDZXJ0aWZpY2F0ZSBBdXRob3JpdHkw
ggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDub7S9eeKPCCGeOARBJe+r
WxxTkqxtnt3CxC5FlAM1iGd0V+PfjLindo8796jE2yljDpFoNoqXjopxaAkH5OjU
Dk/41itMpBb570OYj7OeUt9tkTmPOL13i0Nj67eT/DBMHAGTthP796EfvyXhdDcs
HqRePGj4S78NuR4uNuip5Kf4D8uCdXw1LSLWwr8L87T8bJVhHlfXBIEyg1J55oNj
z7fLY4sR4r1e6/aN7ZVyKLSsEmLpSjPmgzKuBXWVvYSV2ypcm44uDLiBK0HmOFaf
SZtsdvqKXfcBeYF8wYNABf5x/Qw/zE5gCQ5lRxAvAcAFP4/4s0HvWkJ+We/Slwxl
AgMBAAGjggE0MIIBMDAPBgNVHRMBAf8EBTADAQH/MDkGA1UdHwQyMDAwLqAsoCqG
KGh0dHA6Ly9jcmwucGtpLndlbGxzZmFyZ28uY29tL3dzcHJjYS5jcmwwDgYDVR0P
AQH/BAQDAgHGMB0GA1UdDgQWBBQmlRkQ2eihl5H/3BnZtQQ+0nMKajCBsgYDVR0j
BIGqMIGngBQmlRkQ2eihl5H/3BnZtQQ+0nMKaqGBi6SBiDCBhTELMAkGA1UEBhMC
VVMxIDAeBgNVBAoMF1dlbGxzIEZhcmdvIFdlbGxzU2VjdXJlMRwwGgYDVQQLDBNX
ZWxscyBGYXJnbyBCYW5rIE5BMTYwNAYDVQQDDC1XZWxsc1NlY3VyZSBQdWJsaWMg
Um9vdCBDZXJ0aWZpY2F0ZSBBdXRob3JpdHmCAQEwDQYJKoZIhvcNAQEFBQADggEB
ALkVsUSRzCPIK0134/iaeycNzXK7mQDKfGYZUMbVmO2rvwNa5U3lHshPcZeG1eMd
/ZDJPHV3V3p9+N701NX3leZ0bh08rnyd2wIDBSxxSyU+B+NemvVmFymIGjifz6pB
A4SXa5M4esowRBskRDPQ5NHcKDj0E0M1NSljqHyita04pO2t/caaH/+Xc/77szWn
k4bGdpEA5qxRFsQnMlzbc9qlk1eOPm01JghZ1edE13YgY+esE2fDbbFwRnzVlhE9
iW9dqKHrjQrawx0zbKPqZxmamX9LPYNRKh3KL4YMon4QLSvUFpULB6ouFJJJtylv
2G0xffX8oRAHh84vWdw+WNs=
-----END CERTIFICATE-----

# Issuer: CN=COMODO ECC Certification Authority O=COMODO CA Limited
# Subject: CN=COMODO ECC Certification Authority O=COMODO CA Limited
# Label: "COMODO ECC Certification Authority"
# Serial: 41578283867086692638256921589707938090
# MD5 Fingerprint: 7c:62:ff:74:9d:31:53:5e:68:4a:d5:78:aa:1e:bf:23
# SHA1 Fingerprint: 9f:74:4e:9f:2b:4d:ba:ec:0f:31:2c:50:b6:56:3b:8e:2d:93:c3:11
# SHA256 Fingerprint: 17:93:92:7a:06:14:54:97:89:ad:ce:2f:8f:34:f7:f0:b6:6d:0f:3a:e3:a3:b8:4d:21:ec:15:db:ba:4f:ad:c7
-----BEGIN CERTIFICATE-----
MIICiTCCAg+gAwIBAgIQH0evqmIAcFBUTAGem2OZKjAKBggqhkjOPQQDAzCBhTEL
MAkGA1UEBhMCR0IxGzAZBgNVBAgTEkdyZWF0ZXIgTWFuY2hlc3RlcjEQMA4GA1UE
BxMHU2FsZm9yZDEaMBgGA1UEChMRQ09NT0RPIENBIExpbWl0ZWQxKzApBgNVBAMT
IkNPTU9ETyBFQ0MgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwHhcNMDgwMzA2MDAw
MDAwWhcNMzgwMTE4MjM1OTU5WjCBhTELMAkGA1UEBhMCR0IxGzAZBgNVBAgTEkdy
ZWF0ZXIgTWFuY2hlc3RlcjEQMA4GA1UEBxMHU2FsZm9yZDEaMBgGA1UEChMRQ09N
T0RPIENBIExpbWl0ZWQxKzApBgNVBAMTIkNPTU9ETyBFQ0MgQ2VydGlmaWNhdGlv
biBBdXRob3JpdHkwdjAQBgcqhkjOPQIBBgUrgQQAIgNiAAQDR3svdcmCFYX7deSR
FtSrYpn1PlILBs5BAH+X4QokPB0BBO490o0JlwzgdeT6+3eKKvUDYEs2ixYjFq0J
cfRK9ChQtP6IHG4/bC8vCVlbpVsLM5niwz2J+Wos77LTBumjQjBAMB0GA1UdDgQW
BBR1cacZSBm8nZ3qQUfflMRId5nTeTAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/
BAUwAwEB/zAKBggqhkjOPQQDAwNoADBlAjEA7wNbeqy3eApyt4jf/7VGFAkK+qDm
fQjGGoe9GKhzvSbKYAydzpmfz1wPMOG+FDHqAjAU9JM8SaczepBGR7NjfRObTrdv
GDeAU/7dIOA1mjbRxwG55tzd8/8dLDoWV9mSOdY=
-----END CERTIFICATE-----

# Issuer: CN=IGC/A O=PM/SGDN OU=DCSSI
# Subject: CN=IGC/A O=PM/SGDN OU=DCSSI
# Label: "IGC/A"
# Serial: 245102874772
# MD5 Fingerprint: 0c:7f:dd:6a:f4:2a:b9:c8:9b:bd:20:7e:a9:db:5c:37
# SHA1 Fingerprint: 60:d6:89:74:b5:c2:65:9e:8a:0f:c1:88:7c:88:d2:46:69:1b:18:2c
# SHA256 Fingerprint: b9:be:a7:86:0a:96:2e:a3:61:1d:ab:97:ab:6d:a3:e2:1c:10:68:b9:7d:55:57:5e:d0:e1:12:79:c1:1c:89:32
-----BEGIN CERTIFICATE-----
MIIEAjCCAuqgAwIBAgIFORFFEJQwDQYJKoZIhvcNAQEFBQAwgYUxCzAJBgNVBAYT
AkZSMQ8wDQYDVQQIEwZGcmFuY2UxDjAMBgNVBAcTBVBhcmlzMRAwDgYDVQQKEwdQ
TS9TR0ROMQ4wDAYDVQQLEwVEQ1NTSTEOMAwGA1UEAxMFSUdDL0ExIzAhBgkqhkiG
9w0BCQEWFGlnY2FAc2dkbi5wbS5nb3V2LmZyMB4XDTAyMTIxMzE0MjkyM1oXDTIw
MTAxNzE0MjkyMlowgYUxCzAJBgNVBAYTAkZSMQ8wDQYDVQQIEwZGcmFuY2UxDjAM
BgNVBAcTBVBhcmlzMRAwDgYDVQQKEwdQTS9TR0ROMQ4wDAYDVQQLEwVEQ1NTSTEO
MAwGA1UEAxMFSUdDL0ExIzAhBgkqhkiG9w0BCQEWFGlnY2FAc2dkbi5wbS5nb3V2
LmZyMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAsh/R0GLFMzvABIaI
s9z4iPf930Pfeo2aSVz2TqrMHLmh6yeJ8kbpO0px1R2OLc/mratjUMdUC24SyZA2
xtgv2pGqaMVy/hcKshd+ebUyiHDKcMCWSo7kVc0dJ5S/znIq7Fz5cyD+vfcuiWe4
u0dzEvfRNWk68gq5rv9GQkaiv6GFGvm/5P9JhfejcIYyHF2fYPepraX/z9E0+X1b
F8bc1g4oa8Ld8fUzaJ1O/Id8NhLWo4DoQw1VYZTqZDdH6nfK0LJYBcNdfrGoRpAx
Vs5wKpayMLh35nnAvSk7/ZR3TL0gzUEl4C7HG7vupARB0l2tEmqKm0f7yd1GQOGd
PDPQtQIDAQABo3cwdTAPBgNVHRMBAf8EBTADAQH/MAsGA1UdDwQEAwIBRjAVBgNV
HSAEDjAMMAoGCCqBegF5AQEBMB0GA1UdDgQWBBSjBS8YYFDCiQrdKyFP/45OqDAx
NjAfBgNVHSMEGDAWgBSjBS8YYFDCiQrdKyFP/45OqDAxNjANBgkqhkiG9w0BAQUF
AAOCAQEABdwm2Pp3FURo/C9mOnTgXeQp/wYHE4RKq89toB9RlPhJy3Q2FLwV3duJ
L92PoF189RLrn544pEfMs5bZvpwlqwN+Mw+VgQ39FuCIvjfwbF3QMZsyK10XZZOY
YLxuj7GoPB7ZHPOpJkL5ZB3C55L29B5aqhlSXa/oovdgoPaN8In1buAKBQGVyYsg
Crpa/JosPL3Dt8ldeCUFP1YUmwza+zpI/pdpXsoQhvdOlgQITeywvl3cO45Pwf2a
NjSaTFR+FwNIlQgRHAdvhQh+XU3Endv7rs6y0bO4g2wdsrN58dhwmX7wEwLOXt1R
0982gaEbeC9xs/FZTEYYKKuF0mBWWg==
-----END CERTIFICATE-----

# Issuer: O=SECOM Trust Systems CO.,LTD. OU=Security Communication EV RootCA1
# Subject: O=SECOM Trust Systems CO.,LTD. OU=Security Communication EV RootCA1
# Label: "Security Communication EV RootCA1"
# Serial: 0
# MD5 Fingerprint: 22:2d:a6:01:ea:7c:0a:f7:f0:6c:56:43:3f:77:76:d3
# SHA1 Fingerprint: fe:b8:c4:32:dc:f9:76:9a:ce:ae:3d:d8:90:8f:fd:28:86:65:64:7d
# SHA256 Fingerprint: a2:2d:ba:68:1e:97:37:6e:2d:39:7d:72:8a:ae:3a:9b:62:96:b9:fd:ba:60:bc:2e:11:f6:47:f2:c6:75:fb:37
-----BEGIN CERTIFICATE-----
MIIDfTCCAmWgAwIBAgIBADANBgkqhkiG9w0BAQUFADBgMQswCQYDVQQGEwJKUDEl
MCMGA1UEChMcU0VDT00gVHJ1c3QgU3lzdGVtcyBDTy4sTFRELjEqMCgGA1UECxMh
U2VjdXJpdHkgQ29tbXVuaWNhdGlvbiBFViBSb290Q0ExMB4XDTA3MDYwNjAyMTIz
MloXDTM3MDYwNjAyMTIzMlowYDELMAkGA1UEBhMCSlAxJTAjBgNVBAoTHFNFQ09N
IFRydXN0IFN5c3RlbXMgQ08uLExURC4xKjAoBgNVBAsTIVNlY3VyaXR5IENvbW11
bmljYXRpb24gRVYgUm9vdENBMTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoC
ggEBALx/7FebJOD+nLpCeamIivqA4PUHKUPqjgo0No0c+qe1OXj/l3X3L+SqawSE
RMqm4miO/VVQYg+kcQ7OBzgtQoVQrTyWb4vVog7P3kmJPdZkLjjlHmy1V4qe70gO
zXppFodEtZDkBp2uoQSXWHnvIEqCa4wiv+wfD+mEce3xDuS4GBPMVjZd0ZoeUWs5
bmB2iDQL87PRsJ3KYeJkHcFGB7hj3R4zZbOOCVVSPbW9/wfrrWFVGCypaZhKqkDF
MxRldAD5kd6vA0jFQFTcD4SQaCDFkpbcLuUCRarAX1T4bepJz11sS6/vmsJWXMY1
VkJqMF/Cq/biPT+zyRGPMUzXn0kCAwEAAaNCMEAwHQYDVR0OBBYEFDVK9U2vP9eC
OKyrcWUXdYydVZPmMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/MA0G
CSqGSIb3DQEBBQUAA4IBAQCoh+ns+EBnXcPBZsdAS5f8hxOQWsTvoMpfi7ent/HW
tWS3irO4G8za+6xmiEHO6Pzk2x6Ipu0nUBsCMCRGef4Eh3CXQHPRwMFXGZpppSeZ
q51ihPZRwSzJIxXYKLerJRO1RuGGAv8mjMSIkh1W/hln8lXkgKNrnKt34VFxDSDb
EJrbvXZ5B3eZKK2aXtqxT0QsNY6llsf9g/BYxnnWmHyojf6GPgcWkuF75x3sM3Z+
Qi5KhfmRiWiEA4Glm5q+4zfFVKtWOxgtQaQM+ELbmaDgcm+7XeEWT1MKZPlO9L9O
VL14bIjqv5wTJMJwaaJ/D8g8rQjJsJhAoyrniIPtd490
-----END CERTIFICATE-----

# Issuer: CN=OISTE WISeKey Global Root GA CA O=WISeKey OU=Copyright (c) 2005/OISTE Foundation Endorsed
# Subject: CN=OISTE WISeKey Global Root GA CA O=WISeKey OU=Copyright (c) 2005/OISTE Foundation Endorsed
# Label: "OISTE WISeKey Global Root GA CA"
# Serial: 86718877871133159090080555911823548314
# MD5 Fingerprint: bc:6c:51:33:a7:e9:d3:66:63:54:15:72:1b:21:92:93
# SHA1 Fingerprint: 59:22:a1:e1:5a:ea:16:35:21:f8:98:39:6a:46:46:b0:44:1b:0f:a9
# SHA256 Fingerprint: 41:c9:23:86:6a:b4:ca:d6:b7:ad:57:80:81:58:2e:02:07:97:a6:cb:df:4f:ff:78:ce:83:96:b3:89:37:d7:f5
-----BEGIN CERTIFICATE-----
MIID8TCCAtmgAwIBAgIQQT1yx/RrH4FDffHSKFTfmjANBgkqhkiG9w0BAQUFADCB
ijELMAkGA1UEBhMCQ0gxEDAOBgNVBAoTB1dJU2VLZXkxGzAZBgNVBAsTEkNvcHly
aWdodCAoYykgMjAwNTEiMCAGA1UECxMZT0lTVEUgRm91bmRhdGlvbiBFbmRvcnNl
ZDEoMCYGA1UEAxMfT0lTVEUgV0lTZUtleSBHbG9iYWwgUm9vdCBHQSBDQTAeFw0w
NTEyMTExNjAzNDRaFw0zNzEyMTExNjA5NTFaMIGKMQswCQYDVQQGEwJDSDEQMA4G
A1UEChMHV0lTZUtleTEbMBkGA1UECxMSQ29weXJpZ2h0IChjKSAyMDA1MSIwIAYD
VQQLExlPSVNURSBGb3VuZGF0aW9uIEVuZG9yc2VkMSgwJgYDVQQDEx9PSVNURSBX
SVNlS2V5IEdsb2JhbCBSb290IEdBIENBMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A
MIIBCgKCAQEAy0+zAJs9Nt350UlqaxBJH+zYK7LG+DKBKUOVTJoZIyEVRd7jyBxR
VVuuk+g3/ytr6dTqvirdqFEr12bDYVxgAsj1znJ7O7jyTmUIms2kahnBAbtzptf2
w93NvKSLtZlhuAGio9RN1AU9ka34tAhxZK9w8RxrfvbDd50kc3vkDIzh2TbhmYsF
mQvtRTEJysIA2/dyoJaqlYfQjse2YXMNdmaM3Bu0Y6Kff5MTMPGhJ9vZ/yxViJGg
4E8HsChWjBgbl0SOid3gF27nKu+POQoxhILYQBRJLnpB5Kf+42TMwVlxSywhp1t9
4B3RLoGbw9ho972WG6xwsRYUC9tguSYBBQIDAQABo1EwTzALBgNVHQ8EBAMCAYYw
DwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQUswN+rja8sHnR3JQmthG+IbJphpQw
EAYJKwYBBAGCNxUBBAMCAQAwDQYJKoZIhvcNAQEFBQADggEBAEuh/wuHbrP5wUOx
SPMowB0uyQlB+pQAHKSkq0lPjz0e701vvbyk9vImMMkQyh2I+3QZH4VFvbBsUfk2
ftv1TDI6QU9bR8/oCy22xBmddMVHxjtqD6wU2zz0c5ypBd8A3HR4+vg1YFkCExh8
vPtNsCBtQ7tgMHpnM1zFmdH4LTlSc/uMqpclXHLZCB6rTjzjgTGfA6b7wP4piFXa
hNVQA7bihKOmNqoROgHhGEvWRGizPflTdISzRpFGlgC3gCy24eMQ4tui5yiPAZZi
Fj4A4xylNoEYokxSdsARo27mHbrjWr42U8U+dY+GaSlYU7Wcu2+fXMUY7N0v4ZjJ
/L7fCg0=
-----END CERTIFICATE-----

# Issuer: CN=Microsec e-Szigno Root CA O=Microsec Ltd. OU=e-Szigno CA
# Subject: CN=Microsec e-Szigno Root CA O=Microsec Ltd. OU=e-Szigno CA
# Label: "Microsec e-Szigno Root CA"
# Serial: 272122594155480254301341951808045322001
# MD5 Fingerprint: f0:96:b6:2f:c5:10:d5:67:8e:83:25:32:e8:5e:2e:e5
# SHA1 Fingerprint: 23:88:c9:d3:71:cc:9e:96:3d:ff:7d:3c:a7:ce:fc:d6:25:ec:19:0d
# SHA256 Fingerprint: 32:7a:3d:76:1a:ba:de:a0:34:eb:99:84:06:27:5c:b1:a4:77:6e:fd:ae:2f:df:6d:01:68:ea:1c:4f:55:67:d0
-----BEGIN CERTIFICATE-----
MIIHqDCCBpCgAwIBAgIRAMy4579OKRr9otxmpRwsDxEwDQYJKoZIhvcNAQEFBQAw
cjELMAkGA1UEBhMCSFUxETAPBgNVBAcTCEJ1ZGFwZXN0MRYwFAYDVQQKEw1NaWNy
b3NlYyBMdGQuMRQwEgYDVQQLEwtlLVN6aWdubyBDQTEiMCAGA1UEAxMZTWljcm9z
ZWMgZS1Temlnbm8gUm9vdCBDQTAeFw0wNTA0MDYxMjI4NDRaFw0xNzA0MDYxMjI4
NDRaMHIxCzAJBgNVBAYTAkhVMREwDwYDVQQHEwhCdWRhcGVzdDEWMBQGA1UEChMN
TWljcm9zZWMgTHRkLjEUMBIGA1UECxMLZS1Temlnbm8gQ0ExIjAgBgNVBAMTGU1p
Y3Jvc2VjIGUtU3ppZ25vIFJvb3QgQ0EwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAw
ggEKAoIBAQDtyADVgXvNOABHzNuEwSFpLHSQDCHZU4ftPkNEU6+r+ICbPHiN1I2u
uO/TEdyB5s87lozWbxXGd36hL+BfkrYn13aaHUM86tnsL+4582pnS4uCzyL4ZVX+
LMsvfUh6PXX5qqAnu3jCBspRwn5mS6/NoqdNAoI/gqyFxuEPkEeZlApxcpMqyabA
vjxWTHOSJ/FrtfX9/DAFYJLG65Z+AZHCabEeHXtTRbjcQR/Ji3HWVBTji1R4P770
Yjtb9aPs1ZJ04nQw7wHb4dSrmZsqa/i9phyGI0Jf7Enemotb9HI6QMVJPqW+jqpx
62z69Rrkav17fVVA71hu5tnVvCSrwe+3AgMBAAGjggQ3MIIEMzBnBggrBgEFBQcB
AQRbMFkwKAYIKwYBBQUHMAGGHGh0dHBzOi8vcmNhLmUtc3ppZ25vLmh1L29jc3Aw
LQYIKwYBBQUHMAKGIWh0dHA6Ly93d3cuZS1zemlnbm8uaHUvUm9vdENBLmNydDAP
BgNVHRMBAf8EBTADAQH/MIIBcwYDVR0gBIIBajCCAWYwggFiBgwrBgEEAYGoGAIB
AQEwggFQMCgGCCsGAQUFBwIBFhxodHRwOi8vd3d3LmUtc3ppZ25vLmh1L1NaU1ov
MIIBIgYIKwYBBQUHAgIwggEUHoIBEABBACAAdABhAG4A+gBzAO0AdAB2AOEAbgB5
ACAA6QByAHQAZQBsAG0AZQB6AOkAcwDpAGgAZQB6ACAA6QBzACAAZQBsAGYAbwBn
AGEAZADhAHMA4QBoAG8AegAgAGEAIABTAHoAbwBsAGcA4QBsAHQAYQB0APMAIABT
AHoAbwBsAGcA4QBsAHQAYQB0AOEAcwBpACAAUwB6AGEAYgDhAGwAeQB6AGEAdABh
ACAAcwB6AGUAcgBpAG4AdAAgAGsAZQBsAGwAIABlAGwAagDhAHIAbgBpADoAIABo
AHQAdABwADoALwAvAHcAdwB3AC4AZQAtAHMAegBpAGcAbgBvAC4AaAB1AC8AUwBa
AFMAWgAvMIHIBgNVHR8EgcAwgb0wgbqggbeggbSGIWh0dHA6Ly93d3cuZS1zemln
bm8uaHUvUm9vdENBLmNybIaBjmxkYXA6Ly9sZGFwLmUtc3ppZ25vLmh1L0NOPU1p
Y3Jvc2VjJTIwZS1Temlnbm8lMjBSb290JTIwQ0EsT1U9ZS1Temlnbm8lMjBDQSxP
PU1pY3Jvc2VjJTIwTHRkLixMPUJ1ZGFwZXN0LEM9SFU/Y2VydGlmaWNhdGVSZXZv
Y2F0aW9uTGlzdDtiaW5hcnkwDgYDVR0PAQH/BAQDAgEGMIGWBgNVHREEgY4wgYuB
EGluZm9AZS1zemlnbm8uaHWkdzB1MSMwIQYDVQQDDBpNaWNyb3NlYyBlLVN6aWdu
w7MgUm9vdCBDQTEWMBQGA1UECwwNZS1TemlnbsOzIEhTWjEWMBQGA1UEChMNTWlj
cm9zZWMgS2Z0LjERMA8GA1UEBxMIQnVkYXBlc3QxCzAJBgNVBAYTAkhVMIGsBgNV
HSMEgaQwgaGAFMegSXUWYYTbMUuE0vE3QJDvTtz3oXakdDByMQswCQYDVQQGEwJI
VTERMA8GA1UEBxMIQnVkYXBlc3QxFjAUBgNVBAoTDU1pY3Jvc2VjIEx0ZC4xFDAS
BgNVBAsTC2UtU3ppZ25vIENBMSIwIAYDVQQDExlNaWNyb3NlYyBlLVN6aWdubyBS
b290IENBghEAzLjnv04pGv2i3GalHCwPETAdBgNVHQ4EFgQUx6BJdRZhhNsxS4TS
8TdAkO9O3PcwDQYJKoZIhvcNAQEFBQADggEBANMTnGZjWS7KXHAM/IO8VbH0jgds
ZifOwTsgqRy7RlRw7lrMoHfqaEQn6/Ip3Xep1fvj1KcExJW4C+FEaGAHQzAxQmHl
7tnlJNUb3+FKG6qfx1/4ehHqE5MAyopYse7tDk2016g2JnzgOsHVV4Lxdbb9iV/a
86g4nzUGCM4ilb7N1fy+W955a9x6qWVmvrElWl/tftOsRm1M9DKHtCAE4Gx4sHfR
hUZLphK3dehKyVZs15KrnfVJONJPU+NVkBHbmJbGSfI+9J8b4PeI3CVimUTYc78/
MPMMNz7UwiiAc7EBt51alhQBS6kRnSlqLtBdgcDPsiBDxwPgN05dCtxZICU=
-----END CERTIFICATE-----

# Issuer: CN=Certigna O=Dhimyotis
# Subject: CN=Certigna O=Dhimyotis
# Label: "Certigna"
# Serial: 18364802974209362175
# MD5 Fingerprint: ab:57:a6:5b:7d:42:82:19:b5:d8:58:26:28:5e:fd:ff
# SHA1 Fingerprint: b1:2e:13:63:45:86:a4:6f:1a:b2:60:68:37:58:2d:c4:ac:fd:94:97
# SHA256 Fingerprint: e3:b6:a2:db:2e:d7:ce:48:84:2f:7a:c5:32:41:c7:b7:1d:54:14:4b:fb:40:c1:1f:3f:1d:0b:42:f5:ee:a1:2d
-----BEGIN CERTIFICATE-----
MIIDqDCCApCgAwIBAgIJAP7c4wEPyUj/MA0GCSqGSIb3DQEBBQUAMDQxCzAJBgNV
BAYTAkZSMRIwEAYDVQQKDAlEaGlteW90aXMxETAPBgNVBAMMCENlcnRpZ25hMB4X
DTA3MDYyOTE1MTMwNVoXDTI3MDYyOTE1MTMwNVowNDELMAkGA1UEBhMCRlIxEjAQ
BgNVBAoMCURoaW15b3RpczERMA8GA1UEAwwIQ2VydGlnbmEwggEiMA0GCSqGSIb3
DQEBAQUAA4IBDwAwggEKAoIBAQDIaPHJ1tazNHUmgh7stL7qXOEm7RFHYeGifBZ4
QCHkYJ5ayGPhxLGWkv8YbWkj4Sti993iNi+RB7lIzw7sebYs5zRLcAglozyHGxny
gQcPOJAZ0xH+hrTy0V4eHpbNgGzOOzGTtvKg0KmVEn2lmsxryIRWijOp5yIVUxbw
zBfsV1/pogqYCd7jX5xv3EjjhQsVWqa6n6xI4wmy9/Qy3l40vhx4XUJbzg4ij02Q
130yGLMLLGq/jj8UEYkgDncUtT2UCIf3JR7VsmAA7G8qKCVuKj4YYxclPz5EIBb2
JsglrgVKtOdjLPOMFlN+XPsRGgjBRmKfIrjxwo1p3Po6WAbfAgMBAAGjgbwwgbkw
DwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQUGu3+QTmQtCRZvgHyUtVF9lo53BEw
ZAYDVR0jBF0wW4AUGu3+QTmQtCRZvgHyUtVF9lo53BGhOKQ2MDQxCzAJBgNVBAYT
AkZSMRIwEAYDVQQKDAlEaGlteW90aXMxETAPBgNVBAMMCENlcnRpZ25hggkA/tzj
AQ/JSP8wDgYDVR0PAQH/BAQDAgEGMBEGCWCGSAGG+EIBAQQEAwIABzANBgkqhkiG
9w0BAQUFAAOCAQEAhQMeknH2Qq/ho2Ge6/PAD/Kl1NqV5ta+aDY9fm4fTIrv0Q8h
bV6lUmPOEvjvKtpv6zf+EwLHyzs+ImvaYS5/1HI93TDhHkxAGYwP15zRgzB7mFnc
fca5DClMoTOi62c6ZYTTluLtdkVwj7Ur3vkj1kluPBS1xp81HlDQwY9qcEQCYsuu
HWhBp6pX6FOqB9IG9tUUBguRA3UsbHK1YZWaDYu5Def131TN3ubY1gkIl2PlwS6w
t0QmwCbAr1UwnjvVNioZBPRcHv/PLLf/0P2HQBHVESO7SMAhqaQoLf0V+LBOK/Qw
WyH8EZE0vkHve52Xdf+XlcCWWC/qu0bXu+TZLg==
-----END CERTIFICATE-----

# Issuer: CN=TC TrustCenter Class 2 CA II O=TC TrustCenter GmbH OU=TC TrustCenter Class 2 CA
# Subject: CN=TC TrustCenter Class 2 CA II O=TC TrustCenter GmbH OU=TC TrustCenter Class 2 CA
# Label: "TC TrustCenter Class 2 CA II"
# Serial: 941389028203453866782103406992443
# MD5 Fingerprint: ce:78:33:5c:59:78:01:6e:18:ea:b9:36:a0:b9:2e:23
# SHA1 Fingerprint: ae:50:83:ed:7c:f4:5c:bc:8f:61:c6:21:fe:68:5d:79:42:21:15:6e
# SHA256 Fingerprint: e6:b8:f8:76:64:85:f8:07:ae:7f:8d:ac:16:70:46:1f:07:c0:a1:3e:ef:3a:1f:f7:17:53:8d:7a:ba:d3:91:b4
-----BEGIN CERTIFICATE-----
MIIEqjCCA5KgAwIBAgIOLmoAAQACH9dSISwRXDswDQYJKoZIhvcNAQEFBQAwdjEL
MAkGA1UEBhMCREUxHDAaBgNVBAoTE1RDIFRydXN0Q2VudGVyIEdtYkgxIjAgBgNV
BAsTGVRDIFRydXN0Q2VudGVyIENsYXNzIDIgQ0ExJTAjBgNVBAMTHFRDIFRydXN0
Q2VudGVyIENsYXNzIDIgQ0EgSUkwHhcNMDYwMTEyMTQzODQzWhcNMjUxMjMxMjI1
OTU5WjB2MQswCQYDVQQGEwJERTEcMBoGA1UEChMTVEMgVHJ1c3RDZW50ZXIgR21i
SDEiMCAGA1UECxMZVEMgVHJ1c3RDZW50ZXIgQ2xhc3MgMiBDQTElMCMGA1UEAxMc
VEMgVHJ1c3RDZW50ZXIgQ2xhc3MgMiBDQSBJSTCCASIwDQYJKoZIhvcNAQEBBQAD
ggEPADCCAQoCggEBAKuAh5uO8MN8h9foJIIRszzdQ2Lu+MNF2ujhoF/RKrLqk2jf
tMjWQ+nEdVl//OEd+DFwIxuInie5e/060smp6RQvkL4DUsFJzfb95AhmC1eKokKg
uNV/aVyQMrKXDcpK3EY+AlWJU+MaWss2xgdW94zPEfRMuzBwBJWl9jmM/XOBCH2J
XjIeIqkiRUuwZi4wzJ9l/fzLganx4Duvo4bRierERXlQXa7pIXSSTYtZgo+U4+lK
8edJsBTj9WLL1XK9H7nSn6DNqPoByNkN39r8R52zyFTfSUrxIan+GE7uSNQZu+99
5OKdy1u2bv/jzVrndIIFuoAlOMvkaZ6vQaoahPUCAwEAAaOCATQwggEwMA8GA1Ud
EwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMB0GA1UdDgQWBBTjq1RMgKHbVkO3
kUrL84J6E1wIqzCB7QYDVR0fBIHlMIHiMIHfoIHcoIHZhjVodHRwOi8vd3d3LnRy
dXN0Y2VudGVyLmRlL2NybC92Mi90Y19jbGFzc18yX2NhX0lJLmNybIaBn2xkYXA6
Ly93d3cudHJ1c3RjZW50ZXIuZGUvQ049VEMlMjBUcnVzdENlbnRlciUyMENsYXNz
JTIwMiUyMENBJTIwSUksTz1UQyUyMFRydXN0Q2VudGVyJTIwR21iSCxPVT1yb290
Y2VydHMsREM9dHJ1c3RjZW50ZXIsREM9ZGU/Y2VydGlmaWNhdGVSZXZvY2F0aW9u
TGlzdD9iYXNlPzANBgkqhkiG9w0BAQUFAAOCAQEAjNfffu4bgBCzg/XbEeprS6iS
GNn3Bzn1LL4GdXpoUxUc6krtXvwjshOg0wn/9vYua0Fxec3ibf2uWWuFHbhOIprt
ZjluS5TmVfwLG4t3wVMTZonZKNaL80VKY7f9ewthXbhtvsPcW3nS7Yblok2+XnR8
au0WOB9/WIFaGusyiC2y8zl3gK9etmF1KdsjTYjKUCjLhdLTEKJZbtOTVAB6okaV
hgWcqRmY5TFyDADiZ9lA4CQze28suVyrZZ0srHbqNZn1l7kPJOzHdiEoZa5X6AeI
dUpWoNIFOqTmjZKILPPy4cHGYdtBxceb9w4aUUXCYWvcZCcXjFq32nQozZfkvQ==
-----END CERTIFICATE-----

# Issuer: CN=TC TrustCenter Class 3 CA II O=TC TrustCenter GmbH OU=TC TrustCenter Class 3 CA
# Subject: CN=TC TrustCenter Class 3 CA II O=TC TrustCenter GmbH OU=TC TrustCenter Class 3 CA
# Label: "TC TrustCenter Class 3 CA II"
# Serial: 1506523511417715638772220530020799
# MD5 Fingerprint: 56:5f:aa:80:61:12:17:f6:67:21:e6:2b:6d:61:56:8e
# SHA1 Fingerprint: 80:25:ef:f4:6e:70:c8:d4:72:24:65:84:fe:40:3b:8a:8d:6a:db:f5
# SHA256 Fingerprint: 8d:a0:84:fc:f9:9c:e0:77:22:f8:9b:32:05:93:98:06:fa:5c:b8:11:e1:c8:13:f6:a1:08:c7:d3:36:b3:40:8e
-----BEGIN CERTIFICATE-----
MIIEqjCCA5KgAwIBAgIOSkcAAQAC5aBd1j8AUb8wDQYJKoZIhvcNAQEFBQAwdjEL
MAkGA1UEBhMCREUxHDAaBgNVBAoTE1RDIFRydXN0Q2VudGVyIEdtYkgxIjAgBgNV
BAsTGVRDIFRydXN0Q2VudGVyIENsYXNzIDMgQ0ExJTAjBgNVBAMTHFRDIFRydXN0
Q2VudGVyIENsYXNzIDMgQ0EgSUkwHhcNMDYwMTEyMTQ0MTU3WhcNMjUxMjMxMjI1
OTU5WjB2MQswCQYDVQQGEwJERTEcMBoGA1UEChMTVEMgVHJ1c3RDZW50ZXIgR21i
SDEiMCAGA1UECxMZVEMgVHJ1c3RDZW50ZXIgQ2xhc3MgMyBDQTElMCMGA1UEAxMc
VEMgVHJ1c3RDZW50ZXIgQ2xhc3MgMyBDQSBJSTCCASIwDQYJKoZIhvcNAQEBBQAD
ggEPADCCAQoCggEBALTgu1G7OVyLBMVMeRwjhjEQY0NVJz/GRcekPewJDRoeIMJW
Ht4bNwcwIi9v8Qbxq63WyKthoy9DxLCyLfzDlml7forkzMA5EpBCYMnMNWju2l+Q
Vl/NHE1bWEnrDgFPZPosPIlY2C8u4rBo6SI7dYnWRBpl8huXJh0obazovVkdKyT2
1oQDZogkAHhg8fir/gKya/si+zXmFtGt9i4S5Po1auUZuV3bOx4a+9P/FRQI2Alq
ukWdFHlgfa9Aigdzs5OW03Q0jTo3Kd5c7PXuLjHCINy+8U9/I1LZW+Jk2ZyqBwi1
Rb3R0DHBq1SfqdLDYmAD8bs5SpJKPQq5ncWg/jcCAwEAAaOCATQwggEwMA8GA1Ud
EwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMB0GA1UdDgQWBBTUovyfs8PYA9NX
XAek0CSnwPIA1DCB7QYDVR0fBIHlMIHiMIHfoIHcoIHZhjVodHRwOi8vd3d3LnRy
dXN0Y2VudGVyLmRlL2NybC92Mi90Y19jbGFzc18zX2NhX0lJLmNybIaBn2xkYXA6
Ly93d3cudHJ1c3RjZW50ZXIuZGUvQ049VEMlMjBUcnVzdENlbnRlciUyMENsYXNz
JTIwMyUyMENBJTIwSUksTz1UQyUyMFRydXN0Q2VudGVyJTIwR21iSCxPVT1yb290
Y2VydHMsREM9dHJ1c3RjZW50ZXIsREM9ZGU/Y2VydGlmaWNhdGVSZXZvY2F0aW9u
TGlzdD9iYXNlPzANBgkqhkiG9w0BAQUFAAOCAQEANmDkcPcGIEPZIxpC8vijsrlN
irTzwppVMXzEO2eatN9NDoqTSheLG43KieHPOh6sHfGcMrSOWXaiQYUlN6AT0PV8
TtXqluJucsG7Kv5sbviRmEb8yRtXW+rIGjs/sFGYPAfaLFkB2otE6OF0/ado3VS6
g0bsyEa1+K+XwDsJHI/OcpY9M1ZwvJbL2NV9IJqDnxrcOfHFcqMRA/07QlIp2+gB
95tejNaNhk4Z+rwcvsUhpYeeeC422wlxo3I0+GzjBgnyXlal092Y+tTmBvTwtiBj
S+opvaqCZh77gaqnN60TGOaSw4HBM7uIHqHn4rS9MWwOUT1v+5ZWgOI2F9Hc5A==
-----END CERTIFICATE-----

# Issuer: CN=TC TrustCenter Universal CA I O=TC TrustCenter GmbH OU=TC TrustCenter Universal CA
# Subject: CN=TC TrustCenter Universal CA I O=TC TrustCenter GmbH OU=TC TrustCenter Universal CA
# Label: "TC TrustCenter Universal CA I"
# Serial: 601024842042189035295619584734726
# MD5 Fingerprint: 45:e1:a5:72:c5:a9:36:64:40:9e:f5:e4:58:84:67:8c
# SHA1 Fingerprint: 6b:2f:34:ad:89:58:be:62:fd:b0:6b:5c:ce:bb:9d:d9:4f:4e:39:f3
# SHA256 Fingerprint: eb:f3:c0:2a:87:89:b1:fb:7d:51:19:95:d6:63:b7:29:06:d9:13:ce:0d:5e:10:56:8a:8a:77:e2:58:61:67:e7
-----BEGIN CERTIFICATE-----
MIID3TCCAsWgAwIBAgIOHaIAAQAC7LdggHiNtgYwDQYJKoZIhvcNAQEFBQAweTEL
MAkGA1UEBhMCREUxHDAaBgNVBAoTE1RDIFRydXN0Q2VudGVyIEdtYkgxJDAiBgNV
BAsTG1RDIFRydXN0Q2VudGVyIFVuaXZlcnNhbCBDQTEmMCQGA1UEAxMdVEMgVHJ1
c3RDZW50ZXIgVW5pdmVyc2FsIENBIEkwHhcNMDYwMzIyMTU1NDI4WhcNMjUxMjMx
MjI1OTU5WjB5MQswCQYDVQQGEwJERTEcMBoGA1UEChMTVEMgVHJ1c3RDZW50ZXIg
R21iSDEkMCIGA1UECxMbVEMgVHJ1c3RDZW50ZXIgVW5pdmVyc2FsIENBMSYwJAYD
VQQDEx1UQyBUcnVzdENlbnRlciBVbml2ZXJzYWwgQ0EgSTCCASIwDQYJKoZIhvcN
AQEBBQADggEPADCCAQoCggEBAKR3I5ZEr5D0MacQ9CaHnPM42Q9e3s9B6DGtxnSR
JJZ4Hgmgm5qVSkr1YnwCqMqs+1oEdjneX/H5s7/zA1hV0qq34wQi0fiU2iIIAI3T
fCZdzHd55yx4Oagmcw6iXSVphU9VDprvxrlE4Vc93x9UIuVvZaozhDrzznq+VZeu
jRIPFDPiUHDDSYcTvFHe15gSWu86gzOSBnWLknwSaHtwag+1m7Z3W0hZneTvWq3z
wZ7U10VOylY0Ibw+F1tvdwxIAUMpsN0/lm7mlaoMwCC2/T42J5zjXM9OgdwZu5GQ
fezmlwQek8wiSdeXhrYTCjxDI3d+8NzmzSQfO4ObNDqDNOMCAwEAAaNjMGEwHwYD
VR0jBBgwFoAUkqR1LKSevoFE63n8isWVpesQdXMwDwYDVR0TAQH/BAUwAwEB/zAO
BgNVHQ8BAf8EBAMCAYYwHQYDVR0OBBYEFJKkdSyknr6BROt5/IrFlaXrEHVzMA0G
CSqGSIb3DQEBBQUAA4IBAQAo0uCG1eb4e/CX3CJrO5UUVg8RMKWaTzqwOuAGy2X1
7caXJ/4l8lfmXpWMPmRgFVp/Lw0BxbFg/UU1z/CyvwbZ71q+s2IhtNerNXxTPqYn
8aEt2hojnczd7Dwtnic0XQ/CNnm8yUpiLe1r2X1BQ3y2qsrtYbE3ghUJGooWMNjs
ydZHcnhLEEYUjl8Or+zHL6sQ17bxbuyGssLoDZJz3KL0Dzq/YSMQiZxIQG5wALPT
ujdEWBF6AmqI8Dc08BnprNRlc/ZpjGSUOnmFKbAWKwyCPwacx/0QK54PLLae4xW/
2TYcuiUaUj0a7CIMHOCkoj3w6DnPgcB77V0fb8XQC9eY
-----END CERTIFICATE-----

# Issuer: CN=Deutsche Telekom Root CA 2 O=Deutsche Telekom AG OU=T-TeleSec Trust Center
# Subject: CN=Deutsche Telekom Root CA 2 O=Deutsche Telekom AG OU=T-TeleSec Trust Center
# Label: "Deutsche Telekom Root CA 2"
# Serial: 38
# MD5 Fingerprint: 74:01:4a:91:b1:08:c4:58:ce:47:cd:f0:dd:11:53:08
# SHA1 Fingerprint: 85:a4:08:c0:9c:19:3e:5d:51:58:7d:cd:d6:13:30:fd:8c:de:37:bf
# SHA256 Fingerprint: b6:19:1a:50:d0:c3:97:7f:7d:a9:9b:cd:aa:c8:6a:22:7d:ae:b9:67:9e:c7:0b:a3:b0:c9:d9:22:71:c1:70:d3
-----BEGIN CERTIFICATE-----
MIIDnzCCAoegAwIBAgIBJjANBgkqhkiG9w0BAQUFADBxMQswCQYDVQQGEwJERTEc
MBoGA1UEChMTRGV1dHNjaGUgVGVsZWtvbSBBRzEfMB0GA1UECxMWVC1UZWxlU2Vj
IFRydXN0IENlbnRlcjEjMCEGA1UEAxMaRGV1dHNjaGUgVGVsZWtvbSBSb290IENB
IDIwHhcNOTkwNzA5MTIxMTAwWhcNMTkwNzA5MjM1OTAwWjBxMQswCQYDVQQGEwJE
RTEcMBoGA1UEChMTRGV1dHNjaGUgVGVsZWtvbSBBRzEfMB0GA1UECxMWVC1UZWxl
U2VjIFRydXN0IENlbnRlcjEjMCEGA1UEAxMaRGV1dHNjaGUgVGVsZWtvbSBSb290
IENBIDIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCrC6M14IspFLEU
ha88EOQ5bzVdSq7d6mGNlUn0b2SjGmBmpKlAIoTZ1KXleJMOaAGtuU1cOs7TuKhC
QN/Po7qCWWqSG6wcmtoIKyUn+WkjR/Hg6yx6m/UTAtB+NHzCnjwAWav12gz1Mjwr
rFDa1sPeg5TKqAyZMg4ISFZbavva4VhYAUlfckE8FQYBjl2tqriTtM2e66foai1S
NNs671x1Udrb8zH57nGYMsRUFUQM+ZtV7a3fGAigo4aKSe5TBY8ZTNXeWHmb0moc
QqvF1afPaA+W5OFhmHZhyJF81j4A4pFQh+GdCuatl9Idxjp9y7zaAzTVjlsB9WoH
txa2bkp/AgMBAAGjQjBAMB0GA1UdDgQWBBQxw3kbuvVT1xfgiXotF2wKsyudMzAP
BgNVHRMECDAGAQH/AgEFMA4GA1UdDwEB/wQEAwIBBjANBgkqhkiG9w0BAQUFAAOC
AQEAlGRZrTlk5ynrE/5aw4sTV8gEJPB0d8Bg42f76Ymmg7+Wgnxu1MM9756Abrsp
tJh6sTtU6zkXR34ajgv8HzFZMQSyzhfzLMdiNlXiItiJVbSYSKpk+tYcNthEeFpa
IzpXl/V6ME+un2pMSyuOoAPjPuCp1NJ70rOo4nI8rZ7/gFnkm0W09juwzTkZmDLl
6iFhkOQxIY40sfcvNUqFENrnijchvllj4PKFiDFT1FQUhXB59C4Gdyd1Lx+4ivn+
xbrYNuSD7Odlt79jWvNGr4GUN9RBjNYj1h7P9WgbRGOiWrqnNVmh5XAFmw4jV5mU
Cm26OWMohpLzGITY+9HPBVZkVw==
-----END CERTIFICATE-----

# Issuer: CN=ComSign Secured CA O=ComSign
# Subject: CN=ComSign Secured CA O=ComSign
# Label: "ComSign Secured CA"
# Serial: 264725503855295744117309814499492384489
# MD5 Fingerprint: 40:01:25:06:8d:21:43:6a:0e:43:00:9c:e7:43:f3:d5
# SHA1 Fingerprint: f9:cd:0e:2c:da:76:24:c1:8f:bd:f0:f0:ab:b6:45:b8:f7:fe:d5:7a
# SHA256 Fingerprint: 50:79:41:c7:44:60:a0:b4:70:86:22:0d:4e:99:32:57:2a:b5:d1:b5:bb:cb:89:80:ab:1c:b1:76:51:a8:44:d2
-----BEGIN CERTIFICATE-----
MIIDqzCCApOgAwIBAgIRAMcoRwmzuGxFjB36JPU2TukwDQYJKoZIhvcNAQEFBQAw
PDEbMBkGA1UEAxMSQ29tU2lnbiBTZWN1cmVkIENBMRAwDgYDVQQKEwdDb21TaWdu
MQswCQYDVQQGEwJJTDAeFw0wNDAzMjQxMTM3MjBaFw0yOTAzMTYxNTA0NTZaMDwx
GzAZBgNVBAMTEkNvbVNpZ24gU2VjdXJlZCBDQTEQMA4GA1UEChMHQ29tU2lnbjEL
MAkGA1UEBhMCSUwwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDGtWhf
HZQVw6QIVS3joFd67+l0Kru5fFdJGhFeTymHDEjWaueP1H5XJLkGieQcPOqs49oh
gHMhCu95mGwfCP+hUH3ymBvJVG8+pSjsIQQPRbsHPaHA+iqYHU4Gk/v1iDurX8sW
v+bznkqH7Rnqwp9D5PGBpX8QTz7RSmKtUxvLg/8HZaWSLWapW7ha9B20IZFKF3ue
Mv5WJDmyVIRD9YTC2LxBkMyd1mja6YJQqTtoz7VdApRgFrFD2UNd3V2Hbuq7s8lr
9gOUCXDeFhF6K+h2j0kQmHe5Y1yLM5d19guMsqtb3nQgJT/j8xH5h2iGNXHDHYwt
6+UarA9z1YJZQIDTAgMBAAGjgacwgaQwDAYDVR0TBAUwAwEB/zBEBgNVHR8EPTA7
MDmgN6A1hjNodHRwOi8vZmVkaXIuY29tc2lnbi5jby5pbC9jcmwvQ29tU2lnblNl
Y3VyZWRDQS5jcmwwDgYDVR0PAQH/BAQDAgGGMB8GA1UdIwQYMBaAFMFL7XC29z58
ADsAj8c+DkWfHl3sMB0GA1UdDgQWBBTBS+1wtvc+fAA7AI/HPg5Fnx5d7DANBgkq
hkiG9w0BAQUFAAOCAQEAFs/ukhNQq3sUnjO2QiBq1BW9Cav8cujvR3qQrFHBZE7p
iL1DRYHjZiM/EoZNGeQFsOY3wo3aBijJD4mkU6l1P7CW+6tMM1X5eCZGbxs2mPtC
dsGCuY7e+0X5YxtiOzkGynd6qDwJz2w2PQ8KRUtpFhpFfTMDZflScZAmlaxMDPWL
kz/MdXSFmLr/YnpNH4n+rr2UAJm/EaXc4HnFFgt9AmEd6oX5AhVP51qJThRv4zdL
hfXBPGHg/QVBspJ/wx2g0K5SZGBrGMYmnNj1ZOQ2GmKfig8+/21OGVZOIJFsnzQz
OjRXUDpvgV4GxvU+fE6OK85lBi5d0ipTdF7Tbieejw==
-----END CERTIFICATE-----

# Issuer: CN=Cybertrust Global Root O=Cybertrust, Inc
# Subject: CN=Cybertrust Global Root O=Cybertrust, Inc
# Label: "Cybertrust Global Root"
# Serial: 4835703278459682877484360
# MD5 Fingerprint: 72:e4:4a:87:e3:69:40:80:77:ea:bc:e3:f4:ff:f0:e1
# SHA1 Fingerprint: 5f:43:e5:b1:bf:f8:78:8c:ac:1c:c7:ca:4a:9a:c6:22:2b:cc:34:c6
# SHA256 Fingerprint: 96:0a:df:00:63:e9:63:56:75:0c:29:65:dd:0a:08:67:da:0b:9c:bd:6e:77:71:4a:ea:fb:23:49:ab:39:3d:a3
-----BEGIN CERTIFICATE-----
MIIDoTCCAomgAwIBAgILBAAAAAABD4WqLUgwDQYJKoZIhvcNAQEFBQAwOzEYMBYG
A1UEChMPQ3liZXJ0cnVzdCwgSW5jMR8wHQYDVQQDExZDeWJlcnRydXN0IEdsb2Jh
bCBSb290MB4XDTA2MTIxNTA4MDAwMFoXDTIxMTIxNTA4MDAwMFowOzEYMBYGA1UE
ChMPQ3liZXJ0cnVzdCwgSW5jMR8wHQYDVQQDExZDeWJlcnRydXN0IEdsb2JhbCBS
b290MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA+Mi8vRRQZhP/8NN5
7CPytxrHjoXxEnOmGaoQ25yiZXRadz5RfVb23CO21O1fWLE3TdVJDm71aofW0ozS
J8bi/zafmGWgE07GKmSb1ZASzxQG9Dvj1Ci+6A74q05IlG2OlTEQXO2iLb3VOm2y
HLtgwEZLAfVJrn5GitB0jaEMAs7u/OePuGtm839EAL9mJRQr3RAwHQeWP032a7iP
t3sMpTjr3kfb1V05/Iin89cqdPHoWqI7n1C6poxFNcJQZZXcY4Lv3b93TZxiyWNz
FtApD0mpSPCzqrdsxacwOUBdrsTiXSZT8M4cIwhhqJQZugRiQOwfOHB3EgZxpzAY
XSUnpQIDAQABo4GlMIGiMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/
MB0GA1UdDgQWBBS2CHsNesysIEyGVjJez6tuhS1wVzA/BgNVHR8EODA2MDSgMqAw
hi5odHRwOi8vd3d3Mi5wdWJsaWMtdHJ1c3QuY29tL2NybC9jdC9jdHJvb3QuY3Js
MB8GA1UdIwQYMBaAFLYIew16zKwgTIZWMl7Pq26FLXBXMA0GCSqGSIb3DQEBBQUA
A4IBAQBW7wojoFROlZfJ+InaRcHUowAl9B8Tq7ejhVhpwjCt2BWKLePJzYFa+HMj
Wqd8BfP9IjsO0QbE2zZMcwSO5bAi5MXzLqXZI+O4Tkogp24CJJ8iYGd7ix1yCcUx
XOl5n4BHPa2hCwcUPUf/A2kaDAtE52Mlp3+yybh2hO0j9n0Hq0V+09+zv+mKts2o
omcrUtW3ZfA5TGOgkXmTUg9U3YO7n9GPp1Nzw8v/MOx8BLjYRB+TX3EJIrduPuoc
A06dGiBh+4E37F78CkWr1+cXVdCg6mCbpvbjjFspwgZgFJ0tl0ypkxWdYcQBX0jW
WL1WMRJOEcgh4LMRkWXbtKaIOM5V
-----END CERTIFICATE-----

# Issuer: O=Chunghwa Telecom Co., Ltd. OU=ePKI Root Certification Authority
# Subject: O=Chunghwa Telecom Co., Ltd. OU=ePKI Root Certification Authority
# Label: "ePKI Root Certification Authority"
# Serial: 28956088682735189655030529057352760477
# MD5 Fingerprint: 1b:2e:00:ca:26:06:90:3d:ad:fe:6f:15:68:d3:6b:b3
# SHA1 Fingerprint: 67:65:0d:f1:7e:8e:7e:5b:82:40:a4:f4:56:4b:cf:e2:3d:69:c6:f0
# SHA256 Fingerprint: c0:a6:f4:dc:63:a2:4b:fd:cf:54:ef:2a:6a:08:2a:0a:72:de:35:80:3e:2f:f5:ff:52:7a:e5:d8:72:06:df:d5
-----BEGIN CERTIFICATE-----
MIIFsDCCA5igAwIBAgIQFci9ZUdcr7iXAF7kBtK8nTANBgkqhkiG9w0BAQUFADBe
MQswCQYDVQQGEwJUVzEjMCEGA1UECgwaQ2h1bmdod2EgVGVsZWNvbSBDby4sIEx0
ZC4xKjAoBgNVBAsMIWVQS0kgUm9vdCBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTAe
Fw0wNDEyMjAwMjMxMjdaFw0zNDEyMjAwMjMxMjdaMF4xCzAJBgNVBAYTAlRXMSMw
IQYDVQQKDBpDaHVuZ2h3YSBUZWxlY29tIENvLiwgTHRkLjEqMCgGA1UECwwhZVBL
SSBSb290IENlcnRpZmljYXRpb24gQXV0aG9yaXR5MIICIjANBgkqhkiG9w0BAQEF
AAOCAg8AMIICCgKCAgEA4SUP7o3biDN1Z82tH306Tm2d0y8U82N0ywEhajfqhFAH
SyZbCUNsIZ5qyNUD9WBpj8zwIuQf5/dqIjG3LBXy4P4AakP/h2XGtRrBp0xtInAh
ijHyl3SJCRImHJ7K2RKilTza6We/CKBk49ZCt0Xvl/T29de1ShUCWH2YWEtgvM3X
DZoTM1PRYfl61dd4s5oz9wCGzh1NlDivqOx4UXCKXBCDUSH3ET00hl7lSM2XgYI1
TBnsZfZrxQWh7kcT1rMhJ5QQCtkkO7q+RBNGMD+XPNjX12ruOzjjK9SXDrkb5wdJ
fzcq+Xd4z1TtW0ado4AOkUPB1ltfFLqfpo0kR0BZv3I4sjZsN/+Z0V0OWQqraffA
sgRFelQArr5T9rXn4fg8ozHSqf4hUmTFpmfwdQcGlBSBVcYn5AGPF8Fqcde+S/uU
WH1+ETOxQvdibBjWzwloPn9s9h6PYq2lY9sJpx8iQkEeb5mKPtf5P0B6ebClAZLS
nT0IFaUQAS2zMnaolQ2zepr7BxB4EW/hj8e6DyUadCrlHJhBmd8hh+iVBmoKs2pH
dmX2Os+PYhcZewoozRrSgx4hxyy/vv9haLdnG7t4TY3OZ+XkwY63I2binZB1NJip
NiuKmpS5nezMirH4JYlcWrYvjB9teSSnUmjDhDXiZo1jDiVN1Rmy5nk3pyKdVDEC
AwEAAaNqMGgwHQYDVR0OBBYEFB4M97Zn8uGSJglFwFU5Lnc/QkqiMAwGA1UdEwQF
MAMBAf8wOQYEZyoHAAQxMC8wLQIBADAJBgUrDgMCGgUAMAcGBWcqAwAABBRFsMLH
ClZ87lt4DJX5GFPBphzYEDANBgkqhkiG9w0BAQUFAAOCAgEACbODU1kBPpVJufGB
uvl2ICO1J2B01GqZNF5sAFPZn/KmsSQHRGoqxqWOeBLoR9lYGxMqXnmbnwoqZ6Yl
PwZpVnPDimZI+ymBV3QGypzqKOg4ZyYr8dW1P2WT+DZdjo2NQCCHGervJ8A9tDkP
JXtoUHRVnAxZfVo9QZQlUgjgRywVMRnVvwdVxrsStZf0X4OFunHB2WyBEXYKCrC/
gpf36j36+uwtqSiUO1bd0lEursC9CBWMd1I0ltabrNMdjmEPNXubrjlpC2JgQCA2
j6/7Nu4tCEoduL+bXPjqpRugc6bY+G7gMwRfaKonh+3ZwZCc7b3jajWvY9+rGNm6
5ulK6lCKD2GTHuItGeIwlDWSXQ62B68ZgI9HkFFLLk3dheLSClIKF5r8GrBQAuUB
o2M3IUxExJtRmREOc5wGj1QupyheRDmHVi03vYVElOEMSyycw5KFNGHLD7ibSkNS
/jQ6fbjpKdx2qcgw+BRxgMYeNkh0IkFch4LoGHGLQYlE535YW6i4jRPpp2zDR+2z
Gp1iro2C6pSe3VkQw63d4k3jMdXH7OjysP6SHhYKGvzZ8/gntsm+HbRsZJB/9OTE
W9c3rkIO3aQab3yIVMUWbuF6aC74Or8NpDyJO3inTmODBCEIZ43ygknQW/2xzQ+D
hNQ+IIX3Sj0rnP0qCglN6oH4EZw=
-----END CERTIFICATE-----

# Issuer: CN=TÜBİTAK UEKAE Kök Sertifika Hizmet Sağlayıcısı - Sürüm 3 O=Türkiye Bilimsel ve Teknolojik Araştırma Kurumu - TÜBİTAK OU=Ulusal Elektronik ve Kriptoloji Araştırma Enstitüsü - UEKAE/Kamu Sertifikasyon Merkezi
# Subject: CN=TÜBİTAK UEKAE Kök Sertifika Hizmet Sağlayıcısı - Sürüm 3 O=Türkiye Bilimsel ve Teknolojik Araştırma Kurumu - TÜBİTAK OU=Ulusal Elektronik ve Kriptoloji Araştırma Enstitüsü - UEKAE/Kamu Sertifikasyon Merkezi
# Label: "T\xc3\x9c\x42\xC4\xB0TAK UEKAE K\xC3\xB6k Sertifika Hizmet Sa\xC4\x9Flay\xc4\xb1\x63\xc4\xb1s\xc4\xb1 - S\xC3\xBCr\xC3\xBCm 3"
# Serial: 17
# MD5 Fingerprint: ed:41:f5:8c:50:c5:2b:9c:73:e6:ee:6c:eb:c2:a8:26
# SHA1 Fingerprint: 1b:4b:39:61:26:27:6b:64:91:a2:68:6d:d7:02:43:21:2d:1f:1d:96
# SHA256 Fingerprint: e4:c7:34:30:d7:a5:b5:09:25:df:43:37:0a:0d:21:6e:9a:79:b9:d6:db:83:73:a0:c6:9e:b1:cc:31:c7:c5:2a
-----BEGIN CERTIFICATE-----
MIIFFzCCA/+gAwIBAgIBETANBgkqhkiG9w0BAQUFADCCASsxCzAJBgNVBAYTAlRS
MRgwFgYDVQQHDA9HZWJ6ZSAtIEtvY2FlbGkxRzBFBgNVBAoMPlTDvHJraXllIEJp
bGltc2VsIHZlIFRla25vbG9qaWsgQXJhxZ90xLFybWEgS3VydW11IC0gVMOcQsSw
VEFLMUgwRgYDVQQLDD9VbHVzYWwgRWxla3Ryb25payB2ZSBLcmlwdG9sb2ppIEFy
YcWfdMSxcm1hIEVuc3RpdMO8c8O8IC0gVUVLQUUxIzAhBgNVBAsMGkthbXUgU2Vy
dGlmaWthc3lvbiBNZXJrZXppMUowSAYDVQQDDEFUw5xCxLBUQUsgVUVLQUUgS8O2
ayBTZXJ0aWZpa2EgSGl6bWV0IFNhxJ9sYXnEsWPEsXPEsSAtIFPDvHLDvG0gMzAe
Fw0wNzA4MjQxMTM3MDdaFw0xNzA4MjExMTM3MDdaMIIBKzELMAkGA1UEBhMCVFIx
GDAWBgNVBAcMD0dlYnplIC0gS29jYWVsaTFHMEUGA1UECgw+VMO8cmtpeWUgQmls
aW1zZWwgdmUgVGVrbm9sb2ppayBBcmHFn3TEsXJtYSBLdXJ1bXUgLSBUw5xCxLBU
QUsxSDBGBgNVBAsMP1VsdXNhbCBFbGVrdHJvbmlrIHZlIEtyaXB0b2xvamkgQXJh
xZ90xLFybWEgRW5zdGl0w7xzw7wgLSBVRUtBRTEjMCEGA1UECwwaS2FtdSBTZXJ0
aWZpa2FzeW9uIE1lcmtlemkxSjBIBgNVBAMMQVTDnELEsFRBSyBVRUtBRSBLw7Zr
IFNlcnRpZmlrYSBIaXptZXQgU2HEn2xhecSxY8Sxc8SxIC0gU8O8csO8bSAzMIIB
IjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAim1L/xCIOsP2fpTo6iBkcK4h
gb46ezzb8R1Sf1n68yJMlaCQvEhOEav7t7WNeoMojCZG2E6VQIdhn8WebYGHV2yK
O7Rm6sxA/OOqbLLLAdsyv9Lrhc+hDVXDWzhXcLh1xnnRFDDtG1hba+818qEhTsXO
fJlfbLm4IpNQp81McGq+agV/E5wrHur+R84EpW+sky58K5+eeROR6Oqeyjh1jmKw
lZMq5d/pXpduIF9fhHpEORlAHLpVK/swsoHvhOPc7Jg4OQOFCKlUAwUp8MmPi+oL
hmUZEdPpCSPeaJMDyTYcIW7OjGbxmTDY17PDHfiBLqi9ggtm/oLL4eAagsNAgQID
AQABo0IwQDAdBgNVHQ4EFgQUvYiHyY/2pAoLquvF/pEjnatKijIwDgYDVR0PAQH/
BAQDAgEGMA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZIhvcNAQEFBQADggEBAB18+kmP
NOm3JpIWmgV050vQbTlswyb2zrgxvMTfvCr4N5EY3ATIZJkrGG2AA1nJrvhY0D7t
wyOfaTyGOBye79oneNGEN3GKPEs5z35FBtYt2IpNeBLWrcLTy9LQQfMmNkqblWwM
7uXRQydmwYj3erMgbOqwaSvHIOgMA8RBBZniP+Rr+KCGgceExh/VS4ESshYhLBOh
gLJeDEoTniDYYkCrkOpkSi+sDQESeUWoL4cZaMjihccwsnX5OD+ywJO0a+IDRM5n
oN+J1q2MdqMTw5RhK2vZbMEHCiIHhWyFJEapvj+LeISCfiQMnf2BN+MlqO02TpUs
yZyQ2uypQjyttgI=
-----END CERTIFICATE-----

# Issuer: CN=Buypass Class 2 CA 1 O=Buypass AS-983163327
# Subject: CN=Buypass Class 2 CA 1 O=Buypass AS-983163327
# Label: "Buypass Class 2 CA 1"
# Serial: 1
# MD5 Fingerprint: b8:08:9a:f0:03:cc:1b:0d:c8:6c:0b:76:a1:75:64:23
# SHA1 Fingerprint: a0:a1:ab:90:c9:fc:84:7b:3b:12:61:e8:97:7d:5f:d3:22:61:d3:cc
# SHA256 Fingerprint: 0f:4e:9c:dd:26:4b:02:55:50:d1:70:80:63:40:21:4f:e9:44:34:c9:b0:2f:69:7e:c7:10:fc:5f:ea:fb:5e:38
-----BEGIN CERTIFICATE-----
MIIDUzCCAjugAwIBAgIBATANBgkqhkiG9w0BAQUFADBLMQswCQYDVQQGEwJOTzEd
MBsGA1UECgwUQnV5cGFzcyBBUy05ODMxNjMzMjcxHTAbBgNVBAMMFEJ1eXBhc3Mg
Q2xhc3MgMiBDQSAxMB4XDTA2MTAxMzEwMjUwOVoXDTE2MTAxMzEwMjUwOVowSzEL
MAkGA1UEBhMCTk8xHTAbBgNVBAoMFEJ1eXBhc3MgQVMtOTgzMTYzMzI3MR0wGwYD
VQQDDBRCdXlwYXNzIENsYXNzIDIgQ0EgMTCCASIwDQYJKoZIhvcNAQEBBQADggEP
ADCCAQoCggEBAIs8B0XY9t/mx8q6jUPFR42wWsE425KEHK8T1A9vNkYgxC7McXA0
ojTTNy7Y3Tp3L8DrKehc0rWpkTSHIln+zNvnma+WwajHQN2lFYxuyHyXA8vmIPLX
l18xoS830r7uvqmtqEyeIWZDO6i88wmjONVZJMHCR3axiFyCO7srpgTXjAePzdVB
HfCuuCkslFJgNJQ72uA40Z0zPhX0kzLFANq1KWYOOngPIVJfAuWSeyXTkh4vFZ2B
5J2O6O+JzhRMVB0cgRJNcKi+EAUXfh/RuFdV7c27UsKwHnjCTTZoy1YmwVLBvXb3
WNVyfh9EdrsAiR0WnVE1703CVu9r4Iw7DekCAwEAAaNCMEAwDwYDVR0TAQH/BAUw
AwEB/zAdBgNVHQ4EFgQUP42aWYv8e3uco684sDntkHGA1sgwDgYDVR0PAQH/BAQD
AgEGMA0GCSqGSIb3DQEBBQUAA4IBAQAVGn4TirnoB6NLJzKyQJHyIdFkhb5jatLP
gcIV1Xp+DCmsNx4cfHZSldq1fyOhKXdlyTKdqC5Wq2B2zha0jX94wNWZUYN/Xtm+
DKhQ7SLHrQVMdvvt7h5HZPb3J31cKA9FxVxiXqaakZG3Uxcu3K1gnZZkOb1naLKu
BctN518fV4bVIJwo+28TOPX2EZL2fZleHwzoq0QkKXJAPTZSr4xYkHPB7GEseaHs
h7U/2k3ZIQAw3pDaDtMaSKk+hQsUi4y8QZ5q9w5wwDX3OaJdZtB7WZ+oRxKaJyOk
LY4ng5IgodcVf/EuGO70SH8vf/GhGLWhC5SgYiAynB321O+/TIho
-----END CERTIFICATE-----

# Issuer: CN=Buypass Class 3 CA 1 O=Buypass AS-983163327
# Subject: CN=Buypass Class 3 CA 1 O=Buypass AS-983163327
# Label: "Buypass Class 3 CA 1"
# Serial: 2
# MD5 Fingerprint: df:3c:73:59:81:e7:39:50:81:04:4c:34:a2:cb:b3:7b
# SHA1 Fingerprint: 61:57:3a:11:df:0e:d8:7e:d5:92:65:22:ea:d0:56:d7:44:b3:23:71
# SHA256 Fingerprint: b7:b1:2b:17:1f:82:1d:aa:99:0c:d0:fe:50:87:b1:28:44:8b:a8:e5:18:4f:84:c5:1e:02:b5:c8:fb:96:2b:24
-----BEGIN CERTIFICATE-----
MIIDUzCCAjugAwIBAgIBAjANBgkqhkiG9w0BAQUFADBLMQswCQYDVQQGEwJOTzEd
MBsGA1UECgwUQnV5cGFzcyBBUy05ODMxNjMzMjcxHTAbBgNVBAMMFEJ1eXBhc3Mg
Q2xhc3MgMyBDQSAxMB4XDTA1MDUwOTE0MTMwM1oXDTE1MDUwOTE0MTMwM1owSzEL
MAkGA1UEBhMCTk8xHTAbBgNVBAoMFEJ1eXBhc3MgQVMtOTgzMTYzMzI3MR0wGwYD
VQQDDBRCdXlwYXNzIENsYXNzIDMgQ0EgMTCCASIwDQYJKoZIhvcNAQEBBQADggEP
ADCCAQoCggEBAKSO13TZKWTeXx+HgJHqTjnmGcZEC4DVC69TB4sSveZn8AKxifZg
isRbsELRwCGoy+Gb72RRtqfPFfV0gGgEkKBYouZ0plNTVUhjP5JW3SROjvi6K//z
NIqeKNc0n6wv1g/xpC+9UrJJhW05NfBEMJNGJPO251P7vGGvqaMU+8IXF4Rs4HyI
+MkcVyzwPX6UvCWThOiaAJpFBUJXgPROztmuOfbIUxAMZTpHe2DC1vqRycZxbL2R
hzyRhkmr8w+gbCZ2Xhysm3HljbybIR6c1jh+JIAVMYKWsUnTYjdbiAwKYjT+p0h+
mbEwi5A3lRyoH6UsjfRVyNvdWQrCrXig9IsCAwEAAaNCMEAwDwYDVR0TAQH/BAUw
AwEB/zAdBgNVHQ4EFgQUOBTmyPCppAP0Tj4io1vy1uCtQHQwDgYDVR0PAQH/BAQD
AgEGMA0GCSqGSIb3DQEBBQUAA4IBAQABZ6OMySU9E2NdFm/soT4JXJEVKirZgCFP
Bdy7pYmrEzMqnji3jG8CcmPHc3ceCQa6Oyh7pEfJYWsICCD8igWKH7y6xsL+z27s
EzNxZy5p+qksP2bAEllNC1QCkoS72xLvg3BweMhT+t/Gxv/ciC8HwEmdMldg0/L2
mSlf56oBzKwzqBwKu5HEA6BvtjT5htOzdlSY9EqBs1OdTUDs5XcTRa9bqh/YL0yC
e/4qxFi7T/ye/QNlGioOw6UgFpRreaaiErS7GqQjel/wroQk5PMr+4okoyeYZdow
dXb8GZHo2+ubPzK/QJcHJrrM85SFSnonk8+QQtS4Wxam58tAA915
-----END CERTIFICATE-----

# Issuer: CN=EBG Elektronik Sertifika Hizmet Sağlayıcısı O=EBG Bilişim Teknolojileri ve Hizmetleri A.Ş.
# Subject: CN=EBG Elektronik Sertifika Hizmet Sağlayıcısı O=EBG Bilişim Teknolojileri ve Hizmetleri A.Ş.
# Label: "EBG Elektronik Sertifika Hizmet Sa\xC4\x9Flay\xc4\xb1\x63\xc4\xb1s\xc4\xb1"
# Serial: 5525761995591021570
# MD5 Fingerprint: 2c:20:26:9d:cb:1a:4a:00:85:b5:b7:5a:ae:c2:01:37
# SHA1 Fingerprint: 8c:96:ba:eb:dd:2b:07:07:48:ee:30:32:66:a0:f3:98:6e:7c:ae:58
# SHA256 Fingerprint: 35:ae:5b:dd:d8:f7:ae:63:5c:ff:ba:56:82:a8:f0:0b:95:f4:84:62:c7:10:8e:e9:a0:e5:29:2b:07:4a:af:b2
-----BEGIN CERTIFICATE-----
MIIF5zCCA8+gAwIBAgIITK9zQhyOdAIwDQYJKoZIhvcNAQEFBQAwgYAxODA2BgNV
BAMML0VCRyBFbGVrdHJvbmlrIFNlcnRpZmlrYSBIaXptZXQgU2HEn2xhecSxY8Sx
c8SxMTcwNQYDVQQKDC5FQkcgQmlsacWfaW0gVGVrbm9sb2ppbGVyaSB2ZSBIaXpt
ZXRsZXJpIEEuxZ4uMQswCQYDVQQGEwJUUjAeFw0wNjA4MTcwMDIxMDlaFw0xNjA4
MTQwMDMxMDlaMIGAMTgwNgYDVQQDDC9FQkcgRWxla3Ryb25payBTZXJ0aWZpa2Eg
SGl6bWV0IFNhxJ9sYXnEsWPEsXPEsTE3MDUGA1UECgwuRUJHIEJpbGnFn2ltIFRl
a25vbG9qaWxlcmkgdmUgSGl6bWV0bGVyaSBBLsWeLjELMAkGA1UEBhMCVFIwggIi
MA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQDuoIRh0DpqZhAy2DE4f6en5f2h
4fuXd7hxlugTlkaDT7byX3JWbhNgpQGR4lvFzVcfd2NR/y8927k/qqk153nQ9dAk
tiHq6yOU/im/+4mRDGSaBUorzAzu8T2bgmmkTPiab+ci2hC6X5L8GCcKqKpE+i4s
tPtGmggDg3KriORqcsnlZR9uKg+ds+g75AxuetpX/dfreYteIAbTdgtsApWjluTL
dlHRKJ2hGvxEok3MenaoDT2/F08iiFD9rrbskFBKW5+VQarKD7JK/oCZTqNGFav4
c0JqwmZ2sQomFd2TkuzbqV9UIlKRcF0T6kjsbgNs2d1s/OsNA/+mgxKb8amTD8Um
TDGyY5lhcucqZJnSuOl14nypqZoaqsNW2xCaPINStnuWt6yHd6i58mcLlEOzrz5z
+kI2sSXFCjEmN1ZnuqMLfdb3ic1nobc6HmZP9qBVFCVMLDMNpkGMvQQxahByCp0O
Lna9XvNRiYuoP1Vzv9s6xiQFlpJIqkuNKgPlV5EQ9GooFW5Hd4RcUXSfGenmHmMW
OeMRFeNYGkS9y8RsZteEBt8w9DeiQyJ50hBs37vmExH8nYQKE3vwO9D8owrXieqW
fo1IhR5kX9tUoqzVegJ5a9KK8GfaZXINFHDk6Y54jzJ0fFfy1tb0Nokb+Clsi7n2
l9GkLqq+CxnCRelwXQIDAJ3Zo2MwYTAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB
/wQEAwIBBjAdBgNVHQ4EFgQU587GT/wWZ5b6SqMHwQSny2re2kcwHwYDVR0jBBgw
FoAU587GT/wWZ5b6SqMHwQSny2re2kcwDQYJKoZIhvcNAQEFBQADggIBAJuYml2+
8ygjdsZs93/mQJ7ANtyVDR2tFcU22NU57/IeIl6zgrRdu0waypIN30ckHrMk2pGI
6YNw3ZPX6bqz3xZaPt7gyPvT/Wwp+BVGoGgmzJNSroIBk5DKd8pNSe/iWtkqvTDO
TLKBtjDOWU/aWR1qeqRFsIImgYZ29fUQALjuswnoT4cCB64kXPBfrAowzIpAoHME
wfuJJPaaHFy3PApnNgUIMbOv2AFoKuB4j3TeuFGkjGwgPaL7s9QJ/XvCgKqTbCmY
Iai7FvOpEl90tYeY8pUm3zTvilORiF0alKM/fCL414i6poyWqD1SNGKfAB5UVUJn
xk1Gj7sURT0KlhaOEKGXmdXTMIXM3rRyt7yKPBgpaP3ccQfuJDlq+u2lrDgv+R4Q
DgZxGhBM/nV+/x5XOULK1+EVoVZVWRvRo68R2E7DpSvvkL/A7IITW43WciyTTo9q
Kd+FPNMN4KIYEsxVL0e3p5sC/kH2iExt2qkBR4NkJ2IQgtYSe14DHzSpyZH+r11t
hie3I6p1GMog57AP14kOpmciY/SDQSsGS7tY1dHXt7kQY9iJSrSq3RZj9W6+YKH4
7ejWkE8axsWgKdOnIaj1Wjz3x0miIZpKlVIglnKaZsv30oZDfCK+lvm9AahH3eU7
QPl1K5srRmSGjR70j/sHd9DqSaIcjVIUpgqT
-----END CERTIFICATE-----

# Issuer: O=certSIGN OU=certSIGN ROOT CA
# Subject: O=certSIGN OU=certSIGN ROOT CA
# Label: "certSIGN ROOT CA"
# Serial: 35210227249154
# MD5 Fingerprint: 18:98:c0:d6:e9:3a:fc:f9:b0:f5:0c:f7:4b:01:44:17
# SHA1 Fingerprint: fa:b7:ee:36:97:26:62:fb:2d:b0:2a:f6:bf:03:fd:e8:7c:4b:2f:9b
# SHA256 Fingerprint: ea:a9:62:c4:fa:4a:6b:af:eb:e4:15:19:6d:35:1c:cd:88:8d:4f:53:f3:fa:8a:e6:d7:c4:66:a9:4e:60:42:bb
-----BEGIN CERTIFICATE-----
MIIDODCCAiCgAwIBAgIGIAYFFnACMA0GCSqGSIb3DQEBBQUAMDsxCzAJBgNVBAYT
AlJPMREwDwYDVQQKEwhjZXJ0U0lHTjEZMBcGA1UECxMQY2VydFNJR04gUk9PVCBD
QTAeFw0wNjA3MDQxNzIwMDRaFw0zMTA3MDQxNzIwMDRaMDsxCzAJBgNVBAYTAlJP
MREwDwYDVQQKEwhjZXJ0U0lHTjEZMBcGA1UECxMQY2VydFNJR04gUk9PVCBDQTCC
ASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBALczuX7IJUqOtdu0KBuqV5Do
0SLTZLrTk+jUrIZhQGpgV2hUhE28alQCBf/fm5oqrl0Hj0rDKH/v+yv6efHHrfAQ
UySQi2bJqIirr1qjAOm+ukbuW3N7LBeCgV5iLKECZbO9xSsAfsT8AzNXDe3i+s5d
RdY4zTW2ssHQnIFKquSyAVwdj1+ZxLGt24gh65AIgoDzMKND5pCCrlUoSe1b16kQ
OA7+j0xbm0bqQfWwCHTD0IgztnzXdN/chNFDDnU5oSVAKOp4yw4sLjmdjItuFhwv
JoIQ4uNllAoEwF73XVv4EOLQunpL+943AAAaWyjj0pxzPjKHmKHJUS/X3qwzs08C
AwEAAaNCMEAwDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAcYwHQYDVR0O
BBYEFOCMm9slSbPxfIbWskKHC9BroNnkMA0GCSqGSIb3DQEBBQUAA4IBAQA+0hyJ
LjX8+HXd5n9liPRyTMks1zJO890ZeUe9jjtbkw9QSSQTaxQGcu8J06Gh40CEyecY
MnQ8SG4Pn0vU9x7Tk4ZkVJdjclDVVc/6IJMCopvDI5NOFlV2oHB5bc0hH88vLbwZ
44gx+FkagQnIl6Z0x2DEW8xXjrJ1/RsCCdtZb3KTafcxQdaIOL+Hsr0Wefmq5L6I
Jd1hJyMctTEHBDa0GpC9oHRxUIltvBTjD4au8as+x6AJzKNI0eDbZOeStc+vckNw
i/nDhDwTqn6Sm1dTk/pwwpEOMfmbZ13pljheX7NzTogVZ96edhBiIL5VaZVDADlN
9u6wWk5JRFRYX0KD
-----END CERTIFICATE-----

# Issuer: CN=CNNIC ROOT O=CNNIC
# Subject: CN=CNNIC ROOT O=CNNIC
# Label: "CNNIC ROOT"
# Serial: 1228079105
# MD5 Fingerprint: 21:bc:82:ab:49:c4:13:3b:4b:b2:2b:5c:6b:90:9c:19
# SHA1 Fingerprint: 8b:af:4c:9b:1d:f0:2a:92:f7:da:12:8e:b9:1b:ac:f4:98:60:4b:6f
# SHA256 Fingerprint: e2:83:93:77:3d:a8:45:a6:79:f2:08:0c:c7:fb:44:a3:b7:a1:c3:79:2c:b7:eb:77:29:fd:cb:6a:8d:99:ae:a7
-----BEGIN CERTIFICATE-----
MIIDVTCCAj2gAwIBAgIESTMAATANBgkqhkiG9w0BAQUFADAyMQswCQYDVQQGEwJD
TjEOMAwGA1UEChMFQ05OSUMxEzARBgNVBAMTCkNOTklDIFJPT1QwHhcNMDcwNDE2
MDcwOTE0WhcNMjcwNDE2MDcwOTE0WjAyMQswCQYDVQQGEwJDTjEOMAwGA1UEChMF
Q05OSUMxEzARBgNVBAMTCkNOTklDIFJPT1QwggEiMA0GCSqGSIb3DQEBAQUAA4IB
DwAwggEKAoIBAQDTNfc/c3et6FtzF8LRb+1VvG7q6KR5smzDo+/hn7E7SIX1mlwh
IhAsxYLO2uOabjfhhyzcuQxauohV3/2q2x8x6gHx3zkBwRP9SFIhxFXf2tizVHa6
dLG3fdfA6PZZxU3Iva0fFNrfWEQlMhkqx35+jq44sDB7R3IJMfAw28Mbdim7aXZO
V/kbZKKTVrdvmW7bCgScEeOAH8tjlBAKqeFkgjH5jCftppkA9nCTGPihNIaj3XrC
GHn2emU1z5DrvTOTn1OrczvmmzQgLx3vqR1jGqCA2wMv+SYahtKNu6m+UjqHZ0gN
v7Sg2Ca+I19zN38m5pIEo3/PIKe38zrKy5nLAgMBAAGjczBxMBEGCWCGSAGG+EIB
AQQEAwIABzAfBgNVHSMEGDAWgBRl8jGtKvf33VKWCscCwQ7vptU7ETAPBgNVHRMB
Af8EBTADAQH/MAsGA1UdDwQEAwIB/jAdBgNVHQ4EFgQUZfIxrSr3991SlgrHAsEO
76bVOxEwDQYJKoZIhvcNAQEFBQADggEBAEs17szkrr/Dbq2flTtLP1se31cpolnK
OOK5Gv+e5m4y3R6u6jW39ZORTtpC4cMXYFDy0VwmuYK36m3knITnA3kXr5g9lNvH
ugDnuL8BV8F3RTIMO/G0HAiw/VGgod2aHRM2mm23xzy54cXZF/qD1T0VoDy7Hgvi
yJA/qIYM/PmLXoXLT1tLYhFHxUV8BS9BsZ4QaRuZluBVeftOhpm4lNqGOGqTo+fL
buXf6iFViZx9fX+Y9QCJ7uOEwFyWtcVG6kbghVW2G8kS1sHNzYDzAgE8yGnLRUhj
2JTQ7IUOO04RZfSCjKY9ri4ilAnIXOo8gV0WKgOXFlUJ24pBgp5mmxE=
-----END CERTIFICATE-----

# Issuer: O=Japanese Government OU=ApplicationCA
# Subject: O=Japanese Government OU=ApplicationCA
# Label: "ApplicationCA - Japanese Government"
# Serial: 49
# MD5 Fingerprint: 7e:23:4e:5b:a7:a5:b4:25:e9:00:07:74:11:62:ae:d6
# SHA1 Fingerprint: 7f:8a:b0:cf:d0:51:87:6a:66:f3:36:0f:47:c8:8d:8c:d3:35:fc:74
# SHA256 Fingerprint: 2d:47:43:7d:e1:79:51:21:5a:12:f3:c5:8e:51:c7:29:a5:80:26:ef:1f:cc:0a:5f:b3:d9:dc:01:2f:60:0d:19
-----BEGIN CERTIFICATE-----
MIIDoDCCAoigAwIBAgIBMTANBgkqhkiG9w0BAQUFADBDMQswCQYDVQQGEwJKUDEc
MBoGA1UEChMTSmFwYW5lc2UgR292ZXJubWVudDEWMBQGA1UECxMNQXBwbGljYXRp
b25DQTAeFw0wNzEyMTIxNTAwMDBaFw0xNzEyMTIxNTAwMDBaMEMxCzAJBgNVBAYT
AkpQMRwwGgYDVQQKExNKYXBhbmVzZSBHb3Zlcm5tZW50MRYwFAYDVQQLEw1BcHBs
aWNhdGlvbkNBMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAp23gdE6H
j6UG3mii24aZS2QNcfAKBZuOquHMLtJqO8F6tJdhjYq+xpqcBrSGUeQ3DnR4fl+K
f5Sk10cI/VBaVuRorChzoHvpfxiSQE8tnfWuREhzNgaeZCw7NCPbXCbkcXmP1G55
IrmTwcrNwVbtiGrXoDkhBFcsovW8R0FPXjQilbUfKW1eSvNNcr5BViCH/OlQR9cw
FO5cjFW6WY2H/CPek9AEjP3vbb3QesmlOmpyM8ZKDQUXKi17safY1vC+9D/qDiht
QWEjdnjDuGWk81quzMKq2edY3rZ+nYVunyoKb58DKTCXKB28t89UKU5RMfkntigm
/qJj5kEW8DOYRwIDAQABo4GeMIGbMB0GA1UdDgQWBBRUWssmP3HMlEYNllPqa0jQ
k/5CdTAOBgNVHQ8BAf8EBAMCAQYwWQYDVR0RBFIwUKROMEwxCzAJBgNVBAYTAkpQ
MRgwFgYDVQQKDA/ml6XmnKzlm73mlL/lupwxIzAhBgNVBAsMGuOCouODl+ODquOC
seODvOOCt+ODp+ODs0NBMA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZIhvcNAQEFBQAD
ggEBADlqRHZ3ODrso2dGD/mLBqj7apAxzn7s2tGJfHrrLgy9mTLnsCTWw//1sogJ
hyzjVOGjprIIC8CFqMjSnHH2HZ9g/DgzE+Ge3Atf2hZQKXsvcJEPmbo0NI2VdMV+
eKlmXb3KIXdCEKxmJj3ekav9FfBv7WxfEPjzFvYDio+nEhEMy/0/ecGc/WLuo89U
DNErXxc+4z6/wCs+CZv+iKZ+tJIX/COUgb1up8WMwusRRdv4QcmWdupwX3kSa+Sj
B1oF7ydJzyGfikwJcGapJsErEU4z0g781mzSDjJkaP+tBXhfAx2o45CsJOAPQKdL
rosot4LKGAfmt1t06SAZf7IbiVQ=
-----END CERTIFICATE-----

# Issuer: CN=GeoTrust Primary Certification Authority - G3 O=GeoTrust Inc. OU=(c) 2008 GeoTrust Inc. - For authorized use only
# Subject: CN=GeoTrust Primary Certification Authority - G3 O=GeoTrust Inc. OU=(c) 2008 GeoTrust Inc. - For authorized use only
# Label: "GeoTrust Primary Certification Authority - G3"
# Serial: 28809105769928564313984085209975885599
# MD5 Fingerprint: b5:e8:34:36:c9:10:44:58:48:70:6d:2e:83:d4:b8:05
# SHA1 Fingerprint: 03:9e:ed:b8:0b:e7:a0:3c:69:53:89:3b:20:d2:d9:32:3a:4c:2a:fd
# SHA256 Fingerprint: b4:78:b8:12:25:0d:f8:78:63:5c:2a:a7:ec:7d:15:5e:aa:62:5e:e8:29:16:e2:cd:29:43:61:88:6c:d1:fb:d4
-----BEGIN CERTIFICATE-----
MIID/jCCAuagAwIBAgIQFaxulBmyeUtB9iepwxgPHzANBgkqhkiG9w0BAQsFADCB
mDELMAkGA1UEBhMCVVMxFjAUBgNVBAoTDUdlb1RydXN0IEluYy4xOTA3BgNVBAsT
MChjKSAyMDA4IEdlb1RydXN0IEluYy4gLSBGb3IgYXV0aG9yaXplZCB1c2Ugb25s
eTE2MDQGA1UEAxMtR2VvVHJ1c3QgUHJpbWFyeSBDZXJ0aWZpY2F0aW9uIEF1dGhv
cml0eSAtIEczMB4XDTA4MDQwMjAwMDAwMFoXDTM3MTIwMTIzNTk1OVowgZgxCzAJ
BgNVBAYTAlVTMRYwFAYDVQQKEw1HZW9UcnVzdCBJbmMuMTkwNwYDVQQLEzAoYykg
MjAwOCBHZW9UcnVzdCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxNjA0
BgNVBAMTLUdlb1RydXN0IFByaW1hcnkgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkg
LSBHMzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBANziXmJYHTNXOTIz
+uvLh4yn1ErdBojqZI4xmKU4kB6Yzy5jK/BGvESyiaHAKAxJcCGVn2TAppMSAmUm
hsalifD614SgcK9PGpc/BkTVyetyEH3kMSj7HGHmKAdEc5IiaacDiGydY8hS2pgn
5whMcD60yRLBxWeDXTPzAxHsatBT4tG6NmCUgLthY2xbF37fQJQeqw3CIShwiP/W
JmxsYAQlTlV+fe+/lEjetx3dcI0FX4ilm/LC7urRQEFtYjgdVgbFA0dRIBn8exAL
DmKudlW/X3e+PkkBUz2YJQN2JFodtNuJ6nnltrM7P7pMKEF/BqxqjsHQ9gUdfeZC
huOl1UcCAwEAAaNCMEAwDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAQYw
HQYDVR0OBBYEFMR5yo6hTgMdHNxr2zFblD4/MH8tMA0GCSqGSIb3DQEBCwUAA4IB
AQAtxRPPVoB7eni9n64smefv2t+UXglpp+duaIy9cr5HqQ6XErhK8WTTOd8lNNTB
zU6B8A8ExCSzNJbGpqow32hhc9f5joWJ7w5elShKKiePEI4ufIbEAp7aDHdlDkQN
kv39sxY2+hENHYwOB4lqKVb3cvTdFZx3NWZXqxNT2I7BQMXXExZacse3aQHEerGD
AWh9jUGhlBjBJVz88P6DAod8DQ3PLghcSkANPuyBYeYk28rgDi0Hsj5W3I31QYUH
SJsMC8tJP33st/3LjWeJGqvtux6jAAgIFyqCXDFdRootD4abdNlF+9RAsXqqaC2G
spki4cErx5z481+oghLrGREt
-----END CERTIFICATE-----

# Issuer: CN=thawte Primary Root CA - G2 O=thawte, Inc. OU=(c) 2007 thawte, Inc. - For authorized use only
# Subject: CN=thawte Primary Root CA - G2 O=thawte, Inc. OU=(c) 2007 thawte, Inc. - For authorized use only
# Label: "thawte Primary Root CA - G2"
# Serial: 71758320672825410020661621085256472406
# MD5 Fingerprint: 74:9d:ea:60:24:c4:fd:22:53:3e:cc:3a:72:d9:29:4f
# SHA1 Fingerprint: aa:db:bc:22:23:8f:c4:01:a1:27:bb:38:dd:f4:1d:db:08:9e:f0:12
# SHA256 Fingerprint: a4:31:0d:50:af:18:a6:44:71:90:37:2a:86:af:af:8b:95:1f:fb:43:1d:83:7f:1e:56:88:b4:59:71:ed:15:57
-----BEGIN CERTIFICATE-----
MIICiDCCAg2gAwIBAgIQNfwmXNmET8k9Jj1Xm67XVjAKBggqhkjOPQQDAzCBhDEL
MAkGA1UEBhMCVVMxFTATBgNVBAoTDHRoYXd0ZSwgSW5jLjE4MDYGA1UECxMvKGMp
IDIwMDcgdGhhd3RlLCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxJDAi
BgNVBAMTG3RoYXd0ZSBQcmltYXJ5IFJvb3QgQ0EgLSBHMjAeFw0wNzExMDUwMDAw
MDBaFw0zODAxMTgyMzU5NTlaMIGEMQswCQYDVQQGEwJVUzEVMBMGA1UEChMMdGhh
d3RlLCBJbmMuMTgwNgYDVQQLEy8oYykgMjAwNyB0aGF3dGUsIEluYy4gLSBGb3Ig
YXV0aG9yaXplZCB1c2Ugb25seTEkMCIGA1UEAxMbdGhhd3RlIFByaW1hcnkgUm9v
dCBDQSAtIEcyMHYwEAYHKoZIzj0CAQYFK4EEACIDYgAEotWcgnuVnfFSeIf+iha/
BebfowJPDQfGAFG6DAJSLSKkQjnE/o/qycG+1E3/n3qe4rF8mq2nhglzh9HnmuN6
papu+7qzcMBniKI11KOasf2twu8x+qi58/sIxpHR+ymVo0IwQDAPBgNVHRMBAf8E
BTADAQH/MA4GA1UdDwEB/wQEAwIBBjAdBgNVHQ4EFgQUmtgAMADna3+FGO6Lts6K
DPgR4bswCgYIKoZIzj0EAwMDaQAwZgIxAN344FdHW6fmCsO99YCKlzUNG4k8VIZ3
KMqh9HneteY4sPBlcIx/AlTCv//YoT7ZzwIxAMSNlPzcU9LcnXgWHxUzI1NS41ox
XZ3Krr0TKUQNJ1uo52icEvdYPy5yAlejj6EULg==
-----END CERTIFICATE-----

# Issuer: CN=thawte Primary Root CA - G3 O=thawte, Inc. OU=Certification Services Division/(c) 2008 thawte, Inc. - For authorized use only
# Subject: CN=thawte Primary Root CA - G3 O=thawte, Inc. OU=Certification Services Division/(c) 2008 thawte, Inc. - For authorized use only
# Label: "thawte Primary Root CA - G3"
# Serial: 127614157056681299805556476275995414779
# MD5 Fingerprint: fb:1b:5d:43:8a:94:cd:44:c6:76:f2:43:4b:47:e7:31
# SHA1 Fingerprint: f1:8b:53:8d:1b:e9:03:b6:a6:f0:56:43:5b:17:15:89:ca:f3:6b:f2
# SHA256 Fingerprint: 4b:03:f4:58:07:ad:70:f2:1b:fc:2c:ae:71:c9:fd:e4:60:4c:06:4c:f5:ff:b6:86:ba:e5:db:aa:d7:fd:d3:4c
-----BEGIN CERTIFICATE-----
MIIEKjCCAxKgAwIBAgIQYAGXt0an6rS0mtZLL/eQ+zANBgkqhkiG9w0BAQsFADCB
rjELMAkGA1UEBhMCVVMxFTATBgNVBAoTDHRoYXd0ZSwgSW5jLjEoMCYGA1UECxMf
Q2VydGlmaWNhdGlvbiBTZXJ2aWNlcyBEaXZpc2lvbjE4MDYGA1UECxMvKGMpIDIw
MDggdGhhd3RlLCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxJDAiBgNV
BAMTG3RoYXd0ZSBQcmltYXJ5IFJvb3QgQ0EgLSBHMzAeFw0wODA0MDIwMDAwMDBa
Fw0zNzEyMDEyMzU5NTlaMIGuMQswCQYDVQQGEwJVUzEVMBMGA1UEChMMdGhhd3Rl
LCBJbmMuMSgwJgYDVQQLEx9DZXJ0aWZpY2F0aW9uIFNlcnZpY2VzIERpdmlzaW9u
MTgwNgYDVQQLEy8oYykgMjAwOCB0aGF3dGUsIEluYy4gLSBGb3IgYXV0aG9yaXpl
ZCB1c2Ugb25seTEkMCIGA1UEAxMbdGhhd3RlIFByaW1hcnkgUm9vdCBDQSAtIEcz
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAsr8nLPvb2FvdeHsbnndm
gcs+vHyu86YnmjSjaDFxODNi5PNxZnmxqWWjpYvVj2AtP0LMqmsywCPLLEHd5N/8
YZzic7IilRFDGF/Eth9XbAoFWCLINkw6fKXRz4aviKdEAhN0cXMKQlkC+BsUa0Lf
b1+6a4KinVvnSr0eAXLbS3ToO39/fR8EtCab4LRarEc9VbjXsCZSKAExQGbY2SS9
9irY7CFJXJv2eul/VTV+lmuNk5Mny5K76qxAwJ/C+IDPXfRa3M50hqY+bAtTyr2S
zhkGcuYMXDhpxwTWvGzOW/b3aJzcJRVIiKHpqfiYnODz1TEoYRFsZ5aNOZnLwkUk
OQIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIBBjAdBgNV
HQ4EFgQUrWyqlGCc7eT/+j4KdCtjA/e2Wb8wDQYJKoZIhvcNAQELBQADggEBABpA
2JVlrAmSicY59BDlqQ5mU1143vokkbvnRFHfxhY0Cu9qRFHqKweKA3rD6z8KLFIW
oCtDuSWQP3CpMyVtRRooOyfPqsMpQhvfO0zAMzRbQYi/aytlryjvsvXDqmbOe1bu
t8jLZ8HJnBoYuMTDSQPxYA5QzUbF83d597YV4Djbxy8ooAw/dyZ02SUS2jHaGh7c
KUGRIjxpp7sC8rZcJwOJ9Abqm+RyguOhCcHpABnTPtRwa7pxpqpYrvS76Wy274fM
m7v/OeZWYdMKp8RcTGB7BXcmer/YB1IsYvdwY9k5vG8cwnncdimvzsUsZAReiDZu
MdRAGmI0Nj81Aa6sY6A=
-----END CERTIFICATE-----

# Issuer: CN=GeoTrust Primary Certification Authority - G2 O=GeoTrust Inc. OU=(c) 2007 GeoTrust Inc. - For authorized use only
# Subject: CN=GeoTrust Primary Certification Authority - G2 O=GeoTrust Inc. OU=(c) 2007 GeoTrust Inc. - For authorized use only
# Label: "GeoTrust Primary Certification Authority - G2"
# Serial: 80682863203381065782177908751794619243
# MD5 Fingerprint: 01:5e:d8:6b:bd:6f:3d:8e:a1:31:f8:12:e0:98:73:6a
# SHA1 Fingerprint: 8d:17:84:d5:37:f3:03:7d:ec:70:fe:57:8b:51:9a:99:e6:10:d7:b0
# SHA256 Fingerprint: 5e:db:7a:c4:3b:82:a0:6a:87:61:e8:d7:be:49:79:eb:f2:61:1f:7d:d7:9b:f9:1c:1c:6b:56:6a:21:9e:d7:66
-----BEGIN CERTIFICATE-----
MIICrjCCAjWgAwIBAgIQPLL0SAoA4v7rJDteYD7DazAKBggqhkjOPQQDAzCBmDEL
MAkGA1UEBhMCVVMxFjAUBgNVBAoTDUdlb1RydXN0IEluYy4xOTA3BgNVBAsTMChj
KSAyMDA3IEdlb1RydXN0IEluYy4gLSBGb3IgYXV0aG9yaXplZCB1c2Ugb25seTE2
MDQGA1UEAxMtR2VvVHJ1c3QgUHJpbWFyeSBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0
eSAtIEcyMB4XDTA3MTEwNTAwMDAwMFoXDTM4MDExODIzNTk1OVowgZgxCzAJBgNV
BAYTAlVTMRYwFAYDVQQKEw1HZW9UcnVzdCBJbmMuMTkwNwYDVQQLEzAoYykgMjAw
NyBHZW9UcnVzdCBJbmMuIC0gRm9yIGF1dGhvcml6ZWQgdXNlIG9ubHkxNjA0BgNV
BAMTLUdlb1RydXN0IFByaW1hcnkgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkgLSBH
MjB2MBAGByqGSM49AgEGBSuBBAAiA2IABBWx6P0DFUPlrOuHNxFi79KDNlJ9RVcL
So17VDs6bl8VAsBQps8lL33KSLjHUGMcKiEIfJo22Av+0SbFWDEwKCXzXV2juLal
tJLtbCyf691DiaI8S0iRHVDsJt/WYC69IaNCMEAwDwYDVR0TAQH/BAUwAwEB/zAO
BgNVHQ8BAf8EBAMCAQYwHQYDVR0OBBYEFBVfNVdRVfslsq0DafwBo/q+EVXVMAoG
CCqGSM49BAMDA2cAMGQCMGSWWaboCd6LuvpaiIjwH5HTRqjySkwCY/tsXzjbLkGT
qQ7mndwxHLKgpxgceeHHNgIwOlavmnRs9vuD4DPTCF+hnMJbn0bWtsuRBmOiBucz
rD6ogRLQy7rQkgu2npaqBA+K
-----END CERTIFICATE-----

# Issuer: CN=VeriSign Universal Root Certification Authority O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 2008 VeriSign, Inc. - For authorized use only
# Subject: CN=VeriSign Universal Root Certification Authority O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 2008 VeriSign, Inc. - For authorized use only
# Label: "VeriSign Universal Root Certification Authority"
# Serial: 85209574734084581917763752644031726877
# MD5 Fingerprint: 8e:ad:b5:01:aa:4d:81:e4:8c:1d:d1:e1:14:00:95:19
# SHA1 Fingerprint: 36:79:ca:35:66:87:72:30:4d:30:a5:fb:87:3b:0f:a7:7b:b7:0d:54
# SHA256 Fingerprint: 23:99:56:11:27:a5:71:25:de:8c:ef:ea:61:0d:df:2f:a0:78:b5:c8:06:7f:4e:82:82:90:bf:b8:60:e8:4b:3c
-----BEGIN CERTIFICATE-----
MIIEuTCCA6GgAwIBAgIQQBrEZCGzEyEDDrvkEhrFHTANBgkqhkiG9w0BAQsFADCB
vTELMAkGA1UEBhMCVVMxFzAVBgNVBAoTDlZlcmlTaWduLCBJbmMuMR8wHQYDVQQL
ExZWZXJpU2lnbiBUcnVzdCBOZXR3b3JrMTowOAYDVQQLEzEoYykgMjAwOCBWZXJp
U2lnbiwgSW5jLiAtIEZvciBhdXRob3JpemVkIHVzZSBvbmx5MTgwNgYDVQQDEy9W
ZXJpU2lnbiBVbml2ZXJzYWwgUm9vdCBDZXJ0aWZpY2F0aW9uIEF1dGhvcml0eTAe
Fw0wODA0MDIwMDAwMDBaFw0zNzEyMDEyMzU5NTlaMIG9MQswCQYDVQQGEwJVUzEX
MBUGA1UEChMOVmVyaVNpZ24sIEluYy4xHzAdBgNVBAsTFlZlcmlTaWduIFRydXN0
IE5ldHdvcmsxOjA4BgNVBAsTMShjKSAyMDA4IFZlcmlTaWduLCBJbmMuIC0gRm9y
IGF1dGhvcml6ZWQgdXNlIG9ubHkxODA2BgNVBAMTL1ZlcmlTaWduIFVuaXZlcnNh
bCBSb290IENlcnRpZmljYXRpb24gQXV0aG9yaXR5MIIBIjANBgkqhkiG9w0BAQEF
AAOCAQ8AMIIBCgKCAQEAx2E3XrEBNNti1xWb/1hajCMj1mCOkdeQmIN65lgZOIzF
9uVkhbSicfvtvbnazU0AtMgtc6XHaXGVHzk8skQHnOgO+k1KxCHfKWGPMiJhgsWH
H26MfF8WIFFE0XBPV+rjHOPMee5Y2A7Cs0WTwCznmhcrewA3ekEzeOEz4vMQGn+H
LL729fdC4uW/h2KJXwBL38Xd5HVEMkE6HnFuacsLdUYI0crSK5XQz/u5QGtkjFdN
/BMReYTtXlT2NJ8IAfMQJQYXStrxHXpma5hgZqTZ79IugvHw7wnqRMkVauIDbjPT
rJ9VAMf2CGqUuV/c4DPxhGD5WycRtPwW8rtWaoAljQIDAQABo4GyMIGvMA8GA1Ud
EwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMG0GCCsGAQUFBwEMBGEwX6FdoFsw
WTBXMFUWCWltYWdlL2dpZjAhMB8wBwYFKw4DAhoEFI/l0xqGrI2Oa8PPgGrUSBgs
exkuMCUWI2h0dHA6Ly9sb2dvLnZlcmlzaWduLmNvbS92c2xvZ28uZ2lmMB0GA1Ud
DgQWBBS2d/ppSEefUxLVwuoHMnYH0ZcHGTANBgkqhkiG9w0BAQsFAAOCAQEASvj4
sAPmLGd75JR3Y8xuTPl9Dg3cyLk1uXBPY/ok+myDjEedO2Pzmvl2MpWRsXe8rJq+
seQxIcaBlVZaDrHC1LGmWazxY8u4TB1ZkErvkBYoH1quEPuBUDgMbMzxPcP1Y+Oz
4yHJJDnp/RVmRvQbEdBNc6N9Rvk97ahfYtTxP/jgdFcrGJ2BtMQo2pSXpXDrrB2+
BxHw1dvd5Yzw1TKwg+ZX4o+/vqGqvz0dtdQ46tewXDpPaj+PwGZsY6rp2aQW9IHR
lRQOfc2VNNnSj3BzgXucfr2YYdhFh5iQxeuGMMY1v/D/w1WIg0vvBZIGcfK4mJO3
7M2CYfE45k+XmCpajQ==
-----END CERTIFICATE-----

# Issuer: CN=VeriSign Class 3 Public Primary Certification Authority - G4 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 2007 VeriSign, Inc. - For authorized use only
# Subject: CN=VeriSign Class 3 Public Primary Certification Authority - G4 O=VeriSign, Inc. OU=VeriSign Trust Network/(c) 2007 VeriSign, Inc. - For authorized use only
# Label: "VeriSign Class 3 Public Primary Certification Authority - G4"
# Serial: 63143484348153506665311985501458640051
# MD5 Fingerprint: 3a:52:e1:e7:fd:6f:3a:e3:6f:f3:6f:99:1b:f9:22:41
# SHA1 Fingerprint: 22:d5:d8:df:8f:02:31:d1:8d:f7:9d:b7:cf:8a:2d:64:c9:3f:6c:3a
# SHA256 Fingerprint: 69:dd:d7:ea:90:bb:57:c9:3e:13:5d:c8:5e:a6:fc:d5:48:0b:60:32:39:bd:c4:54:fc:75:8b:2a:26:cf:7f:79
-----BEGIN CERTIFICATE-----
MIIDhDCCAwqgAwIBAgIQL4D+I4wOIg9IZxIokYesszAKBggqhkjOPQQDAzCByjEL
MAkGA1UEBhMCVVMxFzAVBgNVBAoTDlZlcmlTaWduLCBJbmMuMR8wHQYDVQQLExZW
ZXJpU2lnbiBUcnVzdCBOZXR3b3JrMTowOAYDVQQLEzEoYykgMjAwNyBWZXJpU2ln
biwgSW5jLiAtIEZvciBhdXRob3JpemVkIHVzZSBvbmx5MUUwQwYDVQQDEzxWZXJp
U2lnbiBDbGFzcyAzIFB1YmxpYyBQcmltYXJ5IENlcnRpZmljYXRpb24gQXV0aG9y
aXR5IC0gRzQwHhcNMDcxMTA1MDAwMDAwWhcNMzgwMTE4MjM1OTU5WjCByjELMAkG
A1UEBhMCVVMxFzAVBgNVBAoTDlZlcmlTaWduLCBJbmMuMR8wHQYDVQQLExZWZXJp
U2lnbiBUcnVzdCBOZXR3b3JrMTowOAYDVQQLEzEoYykgMjAwNyBWZXJpU2lnbiwg
SW5jLiAtIEZvciBhdXRob3JpemVkIHVzZSBvbmx5MUUwQwYDVQQDEzxWZXJpU2ln
biBDbGFzcyAzIFB1YmxpYyBQcmltYXJ5IENlcnRpZmljYXRpb24gQXV0aG9yaXR5
IC0gRzQwdjAQBgcqhkjOPQIBBgUrgQQAIgNiAASnVnp8Utpkmw4tXNherJI9/gHm
GUo9FANL+mAnINmDiWn6VMaaGF5VKmTeBvaNSjutEDxlPZCIBIngMGGzrl0Bp3ve
fLK+ymVhAIau2o970ImtTR1ZmkGxvEeA3J5iw/mjgbIwga8wDwYDVR0TAQH/BAUw
AwEB/zAOBgNVHQ8BAf8EBAMCAQYwbQYIKwYBBQUHAQwEYTBfoV2gWzBZMFcwVRYJ
aW1hZ2UvZ2lmMCEwHzAHBgUrDgMCGgQUj+XTGoasjY5rw8+AatRIGCx7GS4wJRYj
aHR0cDovL2xvZ28udmVyaXNpZ24uY29tL3ZzbG9nby5naWYwHQYDVR0OBBYEFLMW
kf3upm7ktS5Jj4d4gYDs5bG1MAoGCCqGSM49BAMDA2gAMGUCMGYhDBgmYFo4e1ZC
4Kf8NoRRkSAsdk1DPcQdhCPQrNZ8NQbOzWm9kA3bbEhCHQ6qQgIxAJw9SDkjOVga
FRJZap7v1VmyHVIsmXHNxynfGyphe3HR3vPA5Q06Sqotp9iGKt0uEA==
-----END CERTIFICATE-----

# Issuer: CN=NetLock Arany (Class Gold) Főtanúsítvány O=NetLock Kft. OU=Tanúsítványkiadók (Certification Services)
# Subject: CN=NetLock Arany (Class Gold) Főtanúsítvány O=NetLock Kft. OU=Tanúsítványkiadók (Certification Services)
# Label: "NetLock Arany (Class Gold) Főtanúsítvány"
# Serial: 80544274841616
# MD5 Fingerprint: c5:a1:b7:ff:73:dd:d6:d7:34:32:18:df:fc:3c:ad:88
# SHA1 Fingerprint: 06:08:3f:59:3f:15:a1:04:a0:69:a4:6b:a9:03:d0:06:b7:97:09:91
# SHA256 Fingerprint: 6c:61:da:c3:a2:de:f0:31:50:6b:e0:36:d2:a6:fe:40:19:94:fb:d1:3d:f9:c8:d4:66:59:92:74:c4:46:ec:98
-----BEGIN CERTIFICATE-----
MIIEFTCCAv2gAwIBAgIGSUEs5AAQMA0GCSqGSIb3DQEBCwUAMIGnMQswCQYDVQQG
EwJIVTERMA8GA1UEBwwIQnVkYXBlc3QxFTATBgNVBAoMDE5ldExvY2sgS2Z0LjE3
MDUGA1UECwwuVGFuw7pzw610dsOhbnlraWFkw7NrIChDZXJ0aWZpY2F0aW9uIFNl
cnZpY2VzKTE1MDMGA1UEAwwsTmV0TG9jayBBcmFueSAoQ2xhc3MgR29sZCkgRsWR
dGFuw7pzw610dsOhbnkwHhcNMDgxMjExMTUwODIxWhcNMjgxMjA2MTUwODIxWjCB
pzELMAkGA1UEBhMCSFUxETAPBgNVBAcMCEJ1ZGFwZXN0MRUwEwYDVQQKDAxOZXRM
b2NrIEtmdC4xNzA1BgNVBAsMLlRhbsO6c8OtdHbDoW55a2lhZMOzayAoQ2VydGlm
aWNhdGlvbiBTZXJ2aWNlcykxNTAzBgNVBAMMLE5ldExvY2sgQXJhbnkgKENsYXNz
IEdvbGQpIEbFkXRhbsO6c8OtdHbDoW55MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A
MIIBCgKCAQEAxCRec75LbRTDofTjl5Bu0jBFHjzuZ9lk4BqKf8owyoPjIMHj9DrT
lF8afFttvzBPhCf2nx9JvMaZCpDyD/V/Q4Q3Y1GLeqVw/HpYzY6b7cNGbIRwXdrz
AZAj/E4wqX7hJ2Pn7WQ8oLjJM2P+FpD/sLj916jAwJRDC7bVWaaeVtAkH3B5r9s5
VA1lddkVQZQBr17s9o3x/61k/iCa11zr/qYfCGSji3ZVrR47KGAuhyXoqq8fxmRG
ILdwfzzeSNuWU7c5d+Qa4scWhHaXWy+7GRWF+GmF9ZmnqfI0p6m2pgP8b4Y9VHx2
BJtr+UBdADTHLpl1neWIA6pN+APSQnbAGwIDAKiLo0UwQzASBgNVHRMBAf8ECDAG
AQH/AgEEMA4GA1UdDwEB/wQEAwIBBjAdBgNVHQ4EFgQUzPpnk/C2uNClwB7zU/2M
U9+D15YwDQYJKoZIhvcNAQELBQADggEBAKt/7hwWqZw8UQCgwBEIBaeZ5m8BiFRh
bvG5GK1Krf6BQCOUL/t1fC8oS2IkgYIL9WHxHG64YTjrgfpioTtaYtOUZcTh5m2C
+C8lcLIhJsFyUR+MLMOEkMNaj7rP9KdlpeuY0fsFskZ1FSNqb4VjMIDw1Z4fKRzC
bLBQWV2QWzuoDTDPv31/zvGdg73JRm4gpvlhUbohL3u+pRVjodSVh/GeufOJ8z2F
uLjbvrW5KfnaNwUASZQDhETnv0Mxz3WLJdH0pmT1kvarBes96aULNmLazAZfNou2
XjG4Kvte9nHfRCaexOYNkbQudZWAUWpLMKawYqGT8ZvYzsRjdT9ZR7E=
-----END CERTIFICATE-----

# Issuer: CN=Staat der Nederlanden Root CA - G2 O=Staat der Nederlanden
# Subject: CN=Staat der Nederlanden Root CA - G2 O=Staat der Nederlanden
# Label: "Staat der Nederlanden Root CA - G2"
# Serial: 10000012
# MD5 Fingerprint: 7c:a5:0f:f8:5b:9a:7d:6d:30:ae:54:5a:e3:42:a2:8a
# SHA1 Fingerprint: 59:af:82:79:91:86:c7:b4:75:07:cb:cf:03:57:46:eb:04:dd:b7:16
# SHA256 Fingerprint: 66:8c:83:94:7d:a6:3b:72:4b:ec:e1:74:3c:31:a0:e6:ae:d0:db:8e:c5:b3:1b:e3:77:bb:78:4f:91:b6:71:6f
-----BEGIN CERTIFICATE-----
MIIFyjCCA7KgAwIBAgIEAJiWjDANBgkqhkiG9w0BAQsFADBaMQswCQYDVQQGEwJO
TDEeMBwGA1UECgwVU3RhYXQgZGVyIE5lZGVybGFuZGVuMSswKQYDVQQDDCJTdGFh
dCBkZXIgTmVkZXJsYW5kZW4gUm9vdCBDQSAtIEcyMB4XDTA4MDMyNjExMTgxN1oX
DTIwMDMyNTExMDMxMFowWjELMAkGA1UEBhMCTkwxHjAcBgNVBAoMFVN0YWF0IGRl
ciBOZWRlcmxhbmRlbjErMCkGA1UEAwwiU3RhYXQgZGVyIE5lZGVybGFuZGVuIFJv
b3QgQ0EgLSBHMjCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIBAMVZ5291
qj5LnLW4rJ4L5PnZyqtdj7U5EILXr1HgO+EASGrP2uEGQxGZqhQlEq0i6ABtQ8Sp
uOUfiUtnvWFI7/3S4GCI5bkYYCjDdyutsDeqN95kWSpGV+RLufg3fNU254DBtvPU
Z5uW6M7XxgpT0GtJlvOjCwV3SPcl5XCsMBQgJeN/dVrlSPhOewMHBPqCYYdu8DvE
pMfQ9XQ+pV0aCPKbJdL2rAQmPlU6Yiile7Iwr/g3wtG61jj99O9JMDeZJiFIhQGp
5Rbn3JBV3w/oOM2ZNyFPXfUib2rFEhZgF1XyZWampzCROME4HYYEhLoaJXhena/M
UGDWE4dS7WMfbWV9whUYdMrhfmQpjHLYFhN9C0lK8SgbIHRrxT3dsKpICT0ugpTN
GmXZK4iambwYfp/ufWZ8Pr2UuIHOzZgweMFvZ9C+X+Bo7d7iscksWXiSqt8rYGPy
5V6548r6f1CGPqI0GAwJaCgRHOThuVw+R7oyPxjMW4T182t0xHJ04eOLoEq9jWYv
6q012iDTiIJh8BIitrzQ1aTsr1SIJSQ8p22xcik/Plemf1WvbibG/ufMQFxRRIEK
eN5KzlW/HdXZt1bv8Hb/C3m1r737qWmRRpdogBQ2HbN/uymYNqUg+oJgYjOk7Na6
B6duxc8UpufWkjTYgfX8HV2qXB72o007uPc5AgMBAAGjgZcwgZQwDwYDVR0TAQH/
BAUwAwEB/zBSBgNVHSAESzBJMEcGBFUdIAAwPzA9BggrBgEFBQcCARYxaHR0cDov
L3d3dy5wa2lvdmVyaGVpZC5ubC9wb2xpY2llcy9yb290LXBvbGljeS1HMjAOBgNV
HQ8BAf8EBAMCAQYwHQYDVR0OBBYEFJFoMocVHYnitfGsNig0jQt8YojrMA0GCSqG
SIb3DQEBCwUAA4ICAQCoQUpnKpKBglBu4dfYszk78wIVCVBR7y29JHuIhjv5tLyS
CZa59sCrI2AGeYwRTlHSeYAz+51IvuxBQ4EffkdAHOV6CMqqi3WtFMTC6GY8ggen
5ieCWxjmD27ZUD6KQhgpxrRW/FYQoAUXvQwjf/ST7ZwaUb7dRUG/kSS0H4zpX897
IZmflZ85OkYcbPnNe5yQzSipx6lVu6xiNGI1E0sUOlWDuYaNkqbG9AclVMwWVxJK
gnjIFNkXgiYtXSAfea7+1HAWFpWD2DU5/1JddRwWxRNVz0fMdWVSSt7wsKfkCpYL
+63C4iWEst3kvX5ZbJvw8NjnyvLplzh+ib7M+zkXYT9y2zqR2GUBGR2tUKRXCnxL
vJxxcypFURmFzI79R6d0lR2o0a9OF7FpJsKqeFdbxU2n5Z4FF5TKsl+gSRiNNOkm
bEgeqmiSBeGCc1qb3AdbCG19ndeNIdn8FCCqwkXfP+cAslHkwvgFuXkajDTznlvk
N1trSt8sV4pAWja63XVECDdCcAz+3F4hoKOKwJCcaNpQ5kUQR3i2TtJlycM33+FC
Y7BXN0Ute4qcvwXqZVUz9zkQxSgqIXobisQk+T8VyJoVIPVVYpbtbZNQvOSqeK3Z
ywplh6ZmwcSBo3c6WB4L7oOLnR7SUqTMHW+wmG2UMbX4cQrcufx9MmDm66+KAQ==
-----END CERTIFICATE-----

# Issuer: CN=CA Disig O=Disig a.s.
# Subject: CN=CA Disig O=Disig a.s.
# Label: "CA Disig"
# Serial: 1
# MD5 Fingerprint: 3f:45:96:39:e2:50:87:f7:bb:fe:98:0c:3c:20:98:e6
# SHA1 Fingerprint: 2a:c8:d5:8b:57:ce:bf:2f:49:af:f2:fc:76:8f:51:14:62:90:7a:41
# SHA256 Fingerprint: 92:bf:51:19:ab:ec:ca:d0:b1:33:2d:c4:e1:d0:5f:ba:75:b5:67:90:44:ee:0c:a2:6e:93:1f:74:4f:2f:33:cf
-----BEGIN CERTIFICATE-----
MIIEDzCCAvegAwIBAgIBATANBgkqhkiG9w0BAQUFADBKMQswCQYDVQQGEwJTSzET
MBEGA1UEBxMKQnJhdGlzbGF2YTETMBEGA1UEChMKRGlzaWcgYS5zLjERMA8GA1UE
AxMIQ0EgRGlzaWcwHhcNMDYwMzIyMDEzOTM0WhcNMTYwMzIyMDEzOTM0WjBKMQsw
CQYDVQQGEwJTSzETMBEGA1UEBxMKQnJhdGlzbGF2YTETMBEGA1UEChMKRGlzaWcg
YS5zLjERMA8GA1UEAxMIQ0EgRGlzaWcwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAw
ggEKAoIBAQCS9jHBfYj9mQGp2HvycXXxMcbzdWb6UShGhJd4NLxs/LxFWYgmGErE
Nx+hSkS943EE9UQX4j/8SFhvXJ56CbpRNyIjZkMhsDxkovhqFQ4/61HhVKndBpnX
mjxUizkDPw/Fzsbrg3ICqB9x8y34dQjbYkzo+s7552oftms1grrijxaSfQUMbEYD
XcDtab86wYqg6I7ZuUUohwjstMoVvoLdtUSLLa2GDGhibYVW8qwUYzrG0ZmsNHhW
S8+2rT+MitcE5eN4TPWGqvWP+j1scaMtymfraHtuM6kMgiioTGohQBUgDCZbg8Kp
FhXAJIJdKxatymP2dACw30PEEGBWZ2NFAgMBAAGjgf8wgfwwDwYDVR0TAQH/BAUw
AwEB/zAdBgNVHQ4EFgQUjbJJaJ1yCCW5wCf1UJNWSEZx+Y8wDgYDVR0PAQH/BAQD
AgEGMDYGA1UdEQQvMC2BE2Nhb3BlcmF0b3JAZGlzaWcuc2uGFmh0dHA6Ly93d3cu
ZGlzaWcuc2svY2EwZgYDVR0fBF8wXTAtoCugKYYnaHR0cDovL3d3dy5kaXNpZy5z
ay9jYS9jcmwvY2FfZGlzaWcuY3JsMCygKqAohiZodHRwOi8vY2EuZGlzaWcuc2sv
Y2EvY3JsL2NhX2Rpc2lnLmNybDAaBgNVHSAEEzARMA8GDSuBHpGT5goAAAABAQEw
DQYJKoZIhvcNAQEFBQADggEBAF00dGFMrzvY/59tWDYcPQuBDRIrRhCA/ec8J9B6
yKm2fnQwM6M6int0wHl5QpNt/7EpFIKrIYwvF/k/Ji/1WcbvgAa3mkkp7M5+cTxq
EEHA9tOasnxakZzArFvITV734VP/Q3f8nktnbNfzg9Gg4H8l37iYC5oyOGwwoPP/
CBUz91BKez6jPiCp3C9WgArtQVCwyfTssuMmRAAOb54GvCKWU3BlxFAKRmukLyeB
EicTXxChds6KezfqwzlhA5WYOudsiCUI/HloDYd9Yvi0X/vF2Ey9WLw/Q1vUHgFN
PGO+I++MzVpQuGhU+QqZMxEA4Z7CRneC9VkGjCFMhwnN5ag=
-----END CERTIFICATE-----

# Issuer: CN=Juur-SK O=AS Sertifitseerimiskeskus
# Subject: CN=Juur-SK O=AS Sertifitseerimiskeskus
# Label: "Juur-SK"
# Serial: 999181308
# MD5 Fingerprint: aa:8e:5d:d9:f8:db:0a:58:b7:8d:26:87:6c:82:35:55
# SHA1 Fingerprint: 40:9d:4b:d9:17:b5:5c:27:b6:9b:64:cb:98:22:44:0d:cd:09:b8:89
# SHA256 Fingerprint: ec:c3:e9:c3:40:75:03:be:e0:91:aa:95:2f:41:34:8f:f8:8b:aa:86:3b:22:64:be:fa:c8:07:90:15:74:e9:39
-----BEGIN CERTIFICATE-----
MIIE5jCCA86gAwIBAgIEO45L/DANBgkqhkiG9w0BAQUFADBdMRgwFgYJKoZIhvcN
AQkBFglwa2lAc2suZWUxCzAJBgNVBAYTAkVFMSIwIAYDVQQKExlBUyBTZXJ0aWZp
dHNlZXJpbWlza2Vza3VzMRAwDgYDVQQDEwdKdXVyLVNLMB4XDTAxMDgzMDE0MjMw
MVoXDTE2MDgyNjE0MjMwMVowXTEYMBYGCSqGSIb3DQEJARYJcGtpQHNrLmVlMQsw
CQYDVQQGEwJFRTEiMCAGA1UEChMZQVMgU2VydGlmaXRzZWVyaW1pc2tlc2t1czEQ
MA4GA1UEAxMHSnV1ci1TSzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEB
AIFxNj4zB9bjMI0TfncyRsvPGbJgMUaXhvSYRqTCZUXP00B841oiqBB4M8yIsdOB
SvZiF3tfTQou0M+LI+5PAk676w7KvRhj6IAcjeEcjT3g/1tf6mTll+g/mX8MCgkz
ABpTpyHhOEvWgxutr2TC+Rx6jGZITWYfGAriPrsfB2WThbkasLnE+w0R9vXW+RvH
LCu3GFH+4Hv2qEivbDtPL+/40UceJlfwUR0zlv/vWT3aTdEVNMfqPxZIe5EcgEMP
PbgFPtGzlc3Yyg/CQ2fbt5PgIoIuvvVoKIO5wTtpeyDaTpxt4brNj3pssAki14sL
2xzVWiZbDcDq5WDQn/413z8CAwEAAaOCAawwggGoMA8GA1UdEwEB/wQFMAMBAf8w
ggEWBgNVHSAEggENMIIBCTCCAQUGCisGAQQBzh8BAQEwgfYwgdAGCCsGAQUFBwIC
MIHDHoHAAFMAZQBlACAAcwBlAHIAdABpAGYAaQBrAGEAYQB0ACAAbwBuACAAdgDk
AGwAagBhAHMAdABhAHQAdQBkACAAQQBTAC0AaQBzACAAUwBlAHIAdABpAGYAaQB0
AHMAZQBlAHIAaQBtAGkAcwBrAGUAcwBrAHUAcwAgAGEAbABhAG0ALQBTAEsAIABz
AGUAcgB0AGkAZgBpAGsAYQBhAHQAaQBkAGUAIABrAGkAbgBuAGkAdABhAG0AaQBz
AGUAawBzMCEGCCsGAQUFBwIBFhVodHRwOi8vd3d3LnNrLmVlL2Nwcy8wKwYDVR0f
BCQwIjAgoB6gHIYaaHR0cDovL3d3dy5zay5lZS9qdXVyL2NybC8wHQYDVR0OBBYE
FASqekej5ImvGs8KQKcYP2/v6X2+MB8GA1UdIwQYMBaAFASqekej5ImvGs8KQKcY
P2/v6X2+MA4GA1UdDwEB/wQEAwIB5jANBgkqhkiG9w0BAQUFAAOCAQEAe8EYlFOi
CfP+JmeaUOTDBS8rNXiRTHyoERF5TElZrMj3hWVcRrs7EKACr81Ptcw2Kuxd/u+g
kcm2k298gFTsxwhwDY77guwqYHhpNjbRxZyLabVAyJRld/JXIWY7zoVAtjNjGr95
HvxcHdMdkxuLDF2FvZkwMhgJkVLpfKG6/2SSmuz+Ne6ML678IIbsSt4beDI3poHS
na9aEhbKmVv8b20OxaAehsmR0FyYgl9jDIpaq9iVpszLita/ZEuOyoqysOkhMp6q
qIWYNIE5ITuoOlIyPfZrN4YGWhWY3PARZv40ILcD9EEQfTmEeZZyY7aWAuVrua0Z
TbvGRNs2yyqcjg==
-----END CERTIFICATE-----

# Issuer: CN=Hongkong Post Root CA 1 O=Hongkong Post
# Subject: CN=Hongkong Post Root CA 1 O=Hongkong Post
# Label: "Hongkong Post Root CA 1"
# Serial: 1000
# MD5 Fingerprint: a8:0d:6f:39:78:b9:43:6d:77:42:6d:98:5a:cc:23:ca
# SHA1 Fingerprint: d6:da:a8:20:8d:09:d2:15:4d:24:b5:2f:cb:34:6e:b2:58:b2:8a:58
# SHA256 Fingerprint: f9:e6:7d:33:6c:51:00:2a:c0:54:c6:32:02:2d:66:dd:a2:e7:e3:ff:f1:0a:d0:61:ed:31:d8:bb:b4:10:cf:b2
-----BEGIN CERTIFICATE-----
MIIDMDCCAhigAwIBAgICA+gwDQYJKoZIhvcNAQEFBQAwRzELMAkGA1UEBhMCSEsx
FjAUBgNVBAoTDUhvbmdrb25nIFBvc3QxIDAeBgNVBAMTF0hvbmdrb25nIFBvc3Qg
Um9vdCBDQSAxMB4XDTAzMDUxNTA1MTMxNFoXDTIzMDUxNTA0NTIyOVowRzELMAkG
A1UEBhMCSEsxFjAUBgNVBAoTDUhvbmdrb25nIFBvc3QxIDAeBgNVBAMTF0hvbmdr
b25nIFBvc3QgUm9vdCBDQSAxMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKC
AQEArP84tulmAknjorThkPlAj3n54r15/gK97iSSHSL22oVyaf7XPwnU3ZG1ApzQ
jVrhVcNQhrkpJsLj2aDxaQMoIIBFIi1WpztUlVYiWR8o3x8gPW2iNr4joLFutbEn
PzlTCeqrauh0ssJlXI6/fMN4hM2eFvz1Lk8gKgifd/PFHsSaUmYeSF7jEAaPIpjh
ZY4bXSNmO7ilMlHIhqqhqZ5/dpTCpmy3QfDVyAY45tQM4vM7TG1QjMSDJ8EThFk9
nnV0ttgCXjqQesBCNnLsak3c78QA3xMYV18meMjWCnl3v/evt3a5pQuEF10Q6m/h
q5URX208o1xNg1vysxmKgIsLhwIDAQABoyYwJDASBgNVHRMBAf8ECDAGAQH/AgED
MA4GA1UdDwEB/wQEAwIBxjANBgkqhkiG9w0BAQUFAAOCAQEADkbVPK7ih9legYsC
mEEIjEy82tvuJxuC52pF7BaLT4Wg87JwvVqWuspube5Gi27nKi6Wsxkz67SfqLI3
7piol7Yutmcn1KZJ/RyTZXaeQi/cImyaT/JaFTmxcdcrUehtHJjA2Sr0oYJ71clB
oiMBdDhViw+5LmeiIAQ32pwL0xch4I+XeTRvhEgCIDMb5jREn5Fw9IBehEPCKdJs
EhTkYY2sEJCehFC78JZvRZ+K88psT/oROhUVRsPNH4NbLUES7VBnQRM9IauUiqpO
fMGx+6fWtScvl6tu4B3i0RwsH0Ti/L6RoZz71ilTc4afU9hDDl3WY4JxHYB0yvbi
AmvZWg==
-----END CERTIFICATE-----

# Issuer: CN=SecureSign RootCA11 O=Japan Certification Services, Inc.
# Subject: CN=SecureSign RootCA11 O=Japan Certification Services, Inc.
# Label: "SecureSign RootCA11"
# Serial: 1
# MD5 Fingerprint: b7:52:74:e2:92:b4:80:93:f2:75:e4:cc:d7:f2:ea:26
# SHA1 Fingerprint: 3b:c4:9f:48:f8:f3:73:a0:9c:1e:bd:f8:5b:b1:c3:65:c7:d8:11:b3
# SHA256 Fingerprint: bf:0f:ee:fb:9e:3a:58:1a:d5:f9:e9:db:75:89:98:57:43:d2:61:08:5c:4d:31:4f:6f:5d:72:59:aa:42:16:12
-----BEGIN CERTIFICATE-----
MIIDbTCCAlWgAwIBAgIBATANBgkqhkiG9w0BAQUFADBYMQswCQYDVQQGEwJKUDEr
MCkGA1UEChMiSmFwYW4gQ2VydGlmaWNhdGlvbiBTZXJ2aWNlcywgSW5jLjEcMBoG
A1UEAxMTU2VjdXJlU2lnbiBSb290Q0ExMTAeFw0wOTA0MDgwNDU2NDdaFw0yOTA0
MDgwNDU2NDdaMFgxCzAJBgNVBAYTAkpQMSswKQYDVQQKEyJKYXBhbiBDZXJ0aWZp
Y2F0aW9uIFNlcnZpY2VzLCBJbmMuMRwwGgYDVQQDExNTZWN1cmVTaWduIFJvb3RD
QTExMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA/XeqpRyQBTvLTJsz
i1oURaTnkBbR31fSIRCkF/3frNYfp+TbfPfs37gD2pRY/V1yfIw/XwFndBWW4wI8
h9uuywGOwvNmxoVF9ALGOrVisq/6nL+k5tSAMJjzDbaTj6nU2DbysPyKyiyhFTOV
MdrAG/LuYpmGYz+/3ZMqg6h2uRMft85OQoWPIucuGvKVCbIFtUROd6EgvanyTgp9
UK31BQ1FT0Zx/Sg+U/sE2C3XZR1KG/rPO7AxmjVuyIsG0wCR8pQIZUyxNAYAeoni
8McDWc/V1uinMrPmmECGxc0nEovMe863ETxiYAcjPitAbpSACW22s293bzUIUPsC
h8U+iQIDAQABo0IwQDAdBgNVHQ4EFgQUW/hNT7KlhtQ60vFjmqC+CfZXt94wDgYD
VR0PAQH/BAQDAgEGMA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZIhvcNAQEFBQADggEB
AKChOBZmLqdWHyGcBvod7bkixTgm2E5P7KN/ed5GIaGHd48HCJqypMWvDzKYC3xm
KbabfSVSSUOrTC4rbnpwrxYO4wJs+0LmGJ1F2FXI6Dvd5+H0LgscNFxsWEr7jIhQ
X5Ucv+2rIrVls4W6ng+4reV6G4pQOh29Dbx7VFALuUKvVaAYga1lme++5Jy/xIWr
QbJUb9wlze144o4MjQlJ3WN7WmmWAiGovVJZ6X01y8hSyn+B/tlr0/cR7SXf+Of5
pPpyl4RTDaXQMhhRdlkUbA/r7F+AjHVDg8OFmP9Mni0N5HeDk061lgeLKBObjBmN
QSdJQO7e5iNEOdyhIta6A/I=
-----END CERTIFICATE-----

# Issuer: CN=ACEDICOM Root O=EDICOM OU=PKI
# Subject: CN=ACEDICOM Root O=EDICOM OU=PKI
# Label: "ACEDICOM Root"
# Serial: 7029493972724711941
# MD5 Fingerprint: 42:81:a0:e2:1c:e3:55:10:de:55:89:42:65:96:22:e6
# SHA1 Fingerprint: e0:b4:32:2e:b2:f6:a5:68:b6:54:53:84:48:18:4a:50:36:87:43:84
# SHA256 Fingerprint: 03:95:0f:b4:9a:53:1f:3e:19:91:94:23:98:df:a9:e0:ea:32:d7:ba:1c:dd:9b:c8:5d:b5:7e:d9:40:0b:43:4a
-----BEGIN CERTIFICATE-----
MIIFtTCCA52gAwIBAgIIYY3HhjsBggUwDQYJKoZIhvcNAQEFBQAwRDEWMBQGA1UE
AwwNQUNFRElDT00gUm9vdDEMMAoGA1UECwwDUEtJMQ8wDQYDVQQKDAZFRElDT00x
CzAJBgNVBAYTAkVTMB4XDTA4MDQxODE2MjQyMloXDTI4MDQxMzE2MjQyMlowRDEW
MBQGA1UEAwwNQUNFRElDT00gUm9vdDEMMAoGA1UECwwDUEtJMQ8wDQYDVQQKDAZF
RElDT00xCzAJBgNVBAYTAkVTMIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKC
AgEA/5KV4WgGdrQsyFhIyv2AVClVYyT/kGWbEHV7w2rbYgIB8hiGtXxaOLHkWLn7
09gtn70yN78sFW2+tfQh0hOR2QetAQXW8713zl9CgQr5auODAKgrLlUTY4HKRxx7
XBZXehuDYAQ6PmXDzQHe3qTWDLqO3tkE7hdWIpuPY/1NFgu3e3eM+SW10W2ZEi5P
Grjm6gSSrj0RuVFCPYewMYWveVqc/udOXpJPQ/yrOq2lEiZmueIM15jO1FillUAK
t0SdE3QrwqXrIhWYENiLxQSfHY9g5QYbm8+5eaA9oiM/Qj9r+hwDezCNzmzAv+Yb
X79nuIQZ1RXve8uQNjFiybwCq0Zfm/4aaJQ0PZCOrfbkHQl/Sog4P75n/TSW9R28
MHTLOO7VbKvU/PQAtwBbhTIWdjPp2KOZnQUAqhbm84F9b32qhm2tFXTTxKJxqvQU
fecyuB+81fFOvW8XAjnXDpVCOscAPukmYxHqC9FK/xidstd7LzrZlvvoHpKuE1XI
2Sf23EgbsCTBheN3nZqk8wwRHQ3ItBTutYJXCb8gWH8vIiPYcMt5bMlL8qkqyPyH
K9caUPgn6C9D4zq92Fdx/c6mUlv53U3t5fZvie27k5x2IXXwkkwp9y+cAS7+UEae
ZAwUswdbxcJzbPEHXEUkFDWug/FqTYl6+rPYLWbwNof1K1MCAwEAAaOBqjCBpzAP
BgNVHRMBAf8EBTADAQH/MB8GA1UdIwQYMBaAFKaz4SsrSbbXc6GqlPUB53NlTKxQ
MA4GA1UdDwEB/wQEAwIBhjAdBgNVHQ4EFgQUprPhKytJttdzoaqU9QHnc2VMrFAw
RAYDVR0gBD0wOzA5BgRVHSAAMDEwLwYIKwYBBQUHAgEWI2h0dHA6Ly9hY2VkaWNv
bS5lZGljb21ncm91cC5jb20vZG9jMA0GCSqGSIb3DQEBBQUAA4ICAQDOLAtSUWIm
fQwng4/F9tqgaHtPkl7qpHMyEVNEskTLnewPeUKzEKbHDZ3Ltvo/Onzqv4hTGzz3
gvoFNTPhNahXwOf9jU8/kzJPeGYDdwdY6ZXIfj7QeQCM8htRM5u8lOk6e25SLTKe
I6RF+7YuE7CLGLHdztUdp0J/Vb77W7tH1PwkzQSulgUV1qzOMPPKC8W64iLgpq0i
5ALudBF/TP94HTXa5gI06xgSYXcGCRZj6hitoocf8seACQl1ThCojz2GuHURwCRi
ipZ7SkXp7FnFvmuD5uHorLUwHv4FB4D54SMNUI8FmP8sX+g7tq3PgbUhh8oIKiMn
MCArz+2UW6yyetLHKKGKC5tNSixthT8Jcjxn4tncB7rrZXtaAWPWkFtPF2Y9fwsZ
o5NjEFIqnxQWWOLcpfShFosOkYuByptZ+thrkQdlVV9SH686+5DdaaVbnG0OLLb6
zqylfDJKZ0DcMDQj3dcEI2bw/FWAp/tmGYI1Z2JwOV5vx+qQQEQIHriy1tvuWacN
GHk0vFQYXlPKNFHtRQrmjseCNj6nOGOpMCwXEGCSn1WHElkQwg9naRHMTh5+Spqt
r0CodaxWkHS4oJyleW/c6RrIaQXpuvoDs3zk4E7Czp3otkYNbn5XOmeUwssfnHdK
Z05phkOTOPu220+DkdRgfks+KzgHVZhepA==
-----END CERTIFICATE-----

# Issuer: CN=Microsec e-Szigno Root CA 2009 O=Microsec Ltd.
# Subject: CN=Microsec e-Szigno Root CA 2009 O=Microsec Ltd.
# Label: "Microsec e-Szigno Root CA 2009"
# Serial: 14014712776195784473
# MD5 Fingerprint: f8:49:f4:03:bc:44:2d:83:be:48:69:7d:29:64:fc:b1
# SHA1 Fingerprint: 89:df:74:fe:5c:f4:0f:4a:80:f9:e3:37:7d:54:da:91:e1:01:31:8e
# SHA256 Fingerprint: 3c:5f:81:fe:a5:fa:b8:2c:64:bf:a2:ea:ec:af:cd:e8:e0:77:fc:86:20:a7:ca:e5:37:16:3d:f3:6e:db:f3:78
-----BEGIN CERTIFICATE-----
MIIECjCCAvKgAwIBAgIJAMJ+QwRORz8ZMA0GCSqGSIb3DQEBCwUAMIGCMQswCQYD
VQQGEwJIVTERMA8GA1UEBwwIQnVkYXBlc3QxFjAUBgNVBAoMDU1pY3Jvc2VjIEx0
ZC4xJzAlBgNVBAMMHk1pY3Jvc2VjIGUtU3ppZ25vIFJvb3QgQ0EgMjAwOTEfMB0G
CSqGSIb3DQEJARYQaW5mb0BlLXN6aWduby5odTAeFw0wOTA2MTYxMTMwMThaFw0y
OTEyMzAxMTMwMThaMIGCMQswCQYDVQQGEwJIVTERMA8GA1UEBwwIQnVkYXBlc3Qx
FjAUBgNVBAoMDU1pY3Jvc2VjIEx0ZC4xJzAlBgNVBAMMHk1pY3Jvc2VjIGUtU3pp
Z25vIFJvb3QgQ0EgMjAwOTEfMB0GCSqGSIb3DQEJARYQaW5mb0BlLXN6aWduby5o
dTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAOn4j/NjrdqG2KfgQvvP
kd6mJviZpWNwrZuuyjNAfW2WbqEORO7hE52UQlKavXWFdCyoDh2Tthi3jCyoz/tc
cbna7P7ofo/kLx2yqHWH2Leh5TvPmUpG0IMZfcChEhyVbUr02MelTTMuhTlAdX4U
fIASmFDHQWe4oIBhVKZsTh/gnQ4H6cm6M+f+wFUoLAKApxn1ntxVUwOXewdI/5n7
N4okxFnMUBBjjqqpGrCEGob5X7uxUG6k0QrM1XF+H6cbfPVTbiJfyyvm1HxdrtbC
xkzlBQHZ7Vf8wSN5/PrIJIOV87VqUQHQd9bpEqH5GoP7ghu5sJf0dgYzQ0mg/wu1
+rUCAwEAAaOBgDB+MA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMB0G
A1UdDgQWBBTLD8bfQkPMPcu1SCOhGnqmKrs0aDAfBgNVHSMEGDAWgBTLD8bfQkPM
Pcu1SCOhGnqmKrs0aDAbBgNVHREEFDASgRBpbmZvQGUtc3ppZ25vLmh1MA0GCSqG
SIb3DQEBCwUAA4IBAQDJ0Q5eLtXMs3w+y/w9/w0olZMEyL/azXm4Q5DwpL7v8u8h
mLzU1F0G9u5C7DBsoKqpyvGvivo/C3NqPuouQH4frlRheesuCDfXI/OMn74dseGk
ddug4lQUsbocKaQY9hK6ohQU4zE1yED/t+AFdlfBHFny+L/k7SViXITwfn4fs775
tyERzAMBVnCnEJIeGzSBHq2cGsMEPO0CYdYeBvNfOofyK/FFh+U9rNHHV4S9a67c
2Pm2G2JwCz02yULyMtd6YebS2z3PyKnJm9zbWETXbzivf3jTo60adbocwTZ8jx5t
HMN1Rq41Bab2XD0h7lbwyYIiLXpUq3DDfSJlgnCW
-----END CERTIFICATE-----

# Issuer: CN=e-Guven Kok Elektronik Sertifika Hizmet Saglayicisi O=Elektronik Bilgi Guvenligi A.S.
# Subject: CN=e-Guven Kok Elektronik Sertifika Hizmet Saglayicisi O=Elektronik Bilgi Guvenligi A.S.
# Label: "E-Guven Kok Elektronik Sertifika Hizmet Saglayicisi"
# Serial: 91184789765598910059173000485363494069
# MD5 Fingerprint: 3d:41:29:cb:1e:aa:11:74:cd:5d:b0:62:af:b0:43:5b
# SHA1 Fingerprint: dd:e1:d2:a9:01:80:2e:1d:87:5e:84:b3:80:7e:4b:b1:fd:99:41:34
# SHA256 Fingerprint: e6:09:07:84:65:a4:19:78:0c:b6:ac:4c:1c:0b:fb:46:53:d9:d9:cc:6e:b3:94:6e:b7:f3:d6:99:97:ba:d5:98
-----BEGIN CERTIFICATE-----
MIIDtjCCAp6gAwIBAgIQRJmNPMADJ72cdpW56tustTANBgkqhkiG9w0BAQUFADB1
MQswCQYDVQQGEwJUUjEoMCYGA1UEChMfRWxla3Ryb25payBCaWxnaSBHdXZlbmxp
Z2kgQS5TLjE8MDoGA1UEAxMzZS1HdXZlbiBLb2sgRWxla3Ryb25payBTZXJ0aWZp
a2EgSGl6bWV0IFNhZ2xheWljaXNpMB4XDTA3MDEwNDExMzI0OFoXDTE3MDEwNDEx
MzI0OFowdTELMAkGA1UEBhMCVFIxKDAmBgNVBAoTH0VsZWt0cm9uaWsgQmlsZ2kg
R3V2ZW5saWdpIEEuUy4xPDA6BgNVBAMTM2UtR3V2ZW4gS29rIEVsZWt0cm9uaWsg
U2VydGlmaWthIEhpem1ldCBTYWdsYXlpY2lzaTCCASIwDQYJKoZIhvcNAQEBBQAD
ggEPADCCAQoCggEBAMMSIJ6wXgBljU5Gu4Bc6SwGl9XzcslwuedLZYDBS75+PNdU
MZTe1RK6UxYC6lhj71vY8+0qGqpxSKPcEC1fX+tcS5yWCEIlKBHMilpiAVDV6wlT
L/jDj/6z/P2douNffb7tC+Bg62nsM+3YjfsSSYMAyYuXjDtzKjKzEve5TfL0TW3H
5tYmNwjy2f1rXKPlSFxYvEK+A1qBuhw1DADT9SN+cTAIJjjcJRFHLfO6IxClv7wC
90Nex/6wN1CZew+TzuZDLMN+DfIcQ2Zgy2ExR4ejT669VmxMvLz4Bcpk9Ok0oSy1
c+HCPujIyTQlCFzz7abHlJ+tiEMl1+E5YP6sOVkCAwEAAaNCMEAwDgYDVR0PAQH/
BAQDAgEGMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFJ/uRLOU1fqRTy7ZVZoE
VtstxNulMA0GCSqGSIb3DQEBBQUAA4IBAQB/X7lTW2M9dTLn+sR0GstG30ZpHFLP
qk/CaOv/gKlR6D1id4k9CnU58W5dF4dvaAXBlGzZXd/aslnLpRCKysw5zZ/rTt5S
/wzw9JKp8mxTq5vSR6AfdPebmvEvFZ96ZDAYBzwqD2fK/A+JYZ1lpTzlvBNbCNvj
/+27BrtqBrF6T2XGgv0enIu1De5Iu7i9qgi0+6N8y5/NkHZchpZ4Vwpm+Vganf2X
KWDeEaaQHBkc7gGWIjQ0LpH5t8Qn0Xvmv/uARFoW5evg1Ao4vOSR49XrXMGs3xtq
fJ7lddK2l4fbzIcrQzqECK+rPNv3PGYxhrCdU3nt+CPeQuMtgvEP5fqX
-----END CERTIFICATE-----

# Issuer: CN=GlobalSign O=GlobalSign OU=GlobalSign Root CA - R3
# Subject: CN=GlobalSign O=GlobalSign OU=GlobalSign Root CA - R3
# Label: "GlobalSign Root CA - R3"
# Serial: 4835703278459759426209954
# MD5 Fingerprint: c5:df:b8:49:ca:05:13:55:ee:2d:ba:1a:c3:3e:b0:28
# SHA1 Fingerprint: d6:9b:56:11:48:f0:1c:77:c5:45:78:c1:09:26:df:5b:85:69:76:ad
# SHA256 Fingerprint: cb:b5:22:d7:b7:f1:27:ad:6a:01:13:86:5b:df:1c:d4:10:2e:7d:07:59:af:63:5a:7c:f4:72:0d:c9:63:c5:3b
-----BEGIN CERTIFICATE-----
MIIDXzCCAkegAwIBAgILBAAAAAABIVhTCKIwDQYJKoZIhvcNAQELBQAwTDEgMB4G
A1UECxMXR2xvYmFsU2lnbiBSb290IENBIC0gUjMxEzARBgNVBAoTCkdsb2JhbFNp
Z24xEzARBgNVBAMTCkdsb2JhbFNpZ24wHhcNMDkwMzE4MTAwMDAwWhcNMjkwMzE4
MTAwMDAwWjBMMSAwHgYDVQQLExdHbG9iYWxTaWduIFJvb3QgQ0EgLSBSMzETMBEG
A1UEChMKR2xvYmFsU2lnbjETMBEGA1UEAxMKR2xvYmFsU2lnbjCCASIwDQYJKoZI
hvcNAQEBBQADggEPADCCAQoCggEBAMwldpB5BngiFvXAg7aEyiie/QV2EcWtiHL8
RgJDx7KKnQRfJMsuS+FggkbhUqsMgUdwbN1k0ev1LKMPgj0MK66X17YUhhB5uzsT
gHeMCOFJ0mpiLx9e+pZo34knlTifBtc+ycsmWQ1z3rDI6SYOgxXG71uL0gRgykmm
KPZpO/bLyCiR5Z2KYVc3rHQU3HTgOu5yLy6c+9C7v/U9AOEGM+iCK65TpjoWc4zd
QQ4gOsC0p6Hpsk+QLjJg6VfLuQSSaGjlOCZgdbKfd/+RFO+uIEn8rUAVSNECMWEZ
XriX7613t2Saer9fwRPvm2L7DWzgVGkWqQPabumDk3F2xmmFghcCAwEAAaNCMEAw
DgYDVR0PAQH/BAQDAgEGMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFI/wS3+o
LkUkrk1Q+mOai97i3Ru8MA0GCSqGSIb3DQEBCwUAA4IBAQBLQNvAUKr+yAzv95ZU
RUm7lgAJQayzE4aGKAczymvmdLm6AC2upArT9fHxD4q/c2dKg8dEe3jgr25sbwMp
jjM5RcOO5LlXbKr8EpbsU8Yt5CRsuZRj+9xTaGdWPoO4zzUhw8lo/s7awlOqzJCK
6fBdRoyV3XpYKBovHd7NADdBj+1EbddTKJd+82cEHhXXipa0095MJ6RMG3NzdvQX
mcIfeg7jLQitChws/zyrVQ4PkX4268NXSb7hLi18YIvDQVETI53O9zJrlAGomecs
Mx86OyXShkDOOyyGeMlhLxS67ttVb9+E7gUJTb0o2HLO02JQZR7rkpeDMdmztcpH
WD9f
-----END CERTIFICATE-----

# Issuer: CN=Autoridad de Certificacion Firmaprofesional CIF A62634068
# Subject: CN=Autoridad de Certificacion Firmaprofesional CIF A62634068
# Label: "Autoridad de Certificacion Firmaprofesional CIF A62634068"
# Serial: 6047274297262753887
# MD5 Fingerprint: 73:3a:74:7a:ec:bb:a3:96:a6:c2:e4:e2:c8:9b:c0:c3
# SHA1 Fingerprint: ae:c5:fb:3f:c8:e1:bf:c4:e5:4f:03:07:5a:9a:e8:00:b7:f7:b6:fa
# SHA256 Fingerprint: 04:04:80:28:bf:1f:28:64:d4:8f:9a:d4:d8:32:94:36:6a:82:88:56:55:3f:3b:14:30:3f:90:14:7f:5d:40:ef
-----BEGIN CERTIFICATE-----
MIIGFDCCA/ygAwIBAgIIU+w77vuySF8wDQYJKoZIhvcNAQEFBQAwUTELMAkGA1UE
BhMCRVMxQjBABgNVBAMMOUF1dG9yaWRhZCBkZSBDZXJ0aWZpY2FjaW9uIEZpcm1h
cHJvZmVzaW9uYWwgQ0lGIEE2MjYzNDA2ODAeFw0wOTA1MjAwODM4MTVaFw0zMDEy
MzEwODM4MTVaMFExCzAJBgNVBAYTAkVTMUIwQAYDVQQDDDlBdXRvcmlkYWQgZGUg
Q2VydGlmaWNhY2lvbiBGaXJtYXByb2Zlc2lvbmFsIENJRiBBNjI2MzQwNjgwggIi
MA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQDKlmuO6vj78aI14H9M2uDDUtd9
thDIAl6zQyrET2qyyhxdKJp4ERppWVevtSBC5IsP5t9bpgOSL/UR5GLXMnE42QQM
cas9UX4PB99jBVzpv5RvwSmCwLTaUbDBPLutN0pcyvFLNg4kq7/DhHf9qFD0sefG
L9ItWY16Ck6WaVICqjaY7Pz6FIMMNx/Jkjd/14Et5cS54D40/mf0PmbR0/RAz15i
NA9wBj4gGFrO93IbJWyTdBSTo3OxDqqHECNZXyAFGUftaI6SEspd/NYrspI8IM/h
X68gvqB2f3bl7BqGYTM+53u0P6APjqK5am+5hyZvQWyIplD9amML9ZMWGxmPsu2b
m8mQ9QEM3xk9Dz44I8kvjwzRAv4bVdZO0I08r0+k8/6vKtMFnXkIoctXMbScyJCy
Z/QYFpM6/EfY0XiWMR+6KwxfXZmtY4laJCB22N/9q06mIqqdXuYnin1oKaPnirja
EbsXLZmdEyRG98Xi2J+Of8ePdG1asuhy9azuJBCtLxTa/y2aRnFHvkLfuwHb9H/T
KI8xWVvTyQKmtFLKbpf7Q8UIJm+K9Lv9nyiqDdVF8xM6HdjAeI9BZzwelGSuewvF
6NkBiDkal4ZkQdU7hwxu+g/GvUgUvzlN1J5Bto+WHWOWk9mVBngxaJ43BjuAiUVh
OSPHG0SjFeUc+JIwuwIDAQABo4HvMIHsMBIGA1UdEwEB/wQIMAYBAf8CAQEwDgYD
VR0PAQH/BAQDAgEGMB0GA1UdDgQWBBRlzeurNR4APn7VdMActHNHDhpkLzCBpgYD
VR0gBIGeMIGbMIGYBgRVHSAAMIGPMC8GCCsGAQUFBwIBFiNodHRwOi8vd3d3LmZp
cm1hcHJvZmVzaW9uYWwuY29tL2NwczBcBggrBgEFBQcCAjBQHk4AUABhAHMAZQBv
ACAAZABlACAAbABhACAAQgBvAG4AYQBuAG8AdgBhACAANAA3ACAAQgBhAHIAYwBl
AGwAbwBuAGEAIAAwADgAMAAxADcwDQYJKoZIhvcNAQEFBQADggIBABd9oPm03cXF
661LJLWhAqvdpYhKsg9VSytXjDvlMd3+xDLx51tkljYyGOylMnfX40S2wBEqgLk9
am58m9Ot/MPWo+ZkKXzR4Tgegiv/J2Wv+xYVxC5xhOW1//qkR71kMrv2JYSiJ0L1
ILDCExARzRAVukKQKtJE4ZYm6zFIEv0q2skGz3QeqUvVhyj5eTSSPi5E6PaPT481
PyWzOdxjKpBrIF/EUhJOlywqrJ2X3kjyo2bbwtKDlaZmp54lD+kLM5FlClrD2VQS
3a/DTg4fJl4N3LON7NWBcN7STyQF82xO9UxJZo3R/9ILJUFI/lGExkKvgATP0H5k
SeTy36LssUzAKh3ntLFlosS88Zj0qnAHY7S42jtM+kAiMFsRpvAFDsYCA0irhpuF
3dvd6qJ2gHN99ZwExEWN57kci57q13XRcrHedUTnQn3iV2t93Jm8PYMo6oCTjcVM
ZcFwgbg4/EMxsvYDNEeyrPsiBsse3RdHHF9mudMaotoRsaS8I8nkvof/uZS2+F0g
StRf571oe2XyFR7SOqkt6dhrJKyXWERHrVkY8SFlcN7ONGCoQPHzPKTDKCOM/icz
Q0CgFzzr6juwcqajuUpLXhZI9LK8yIySxZ2frHI2vDSANGupi5LAuBft7HZT9SQB
jLMi6Et8Vcad+qMUu2WFbm5PEn4KPJ2V
-----END CERTIFICATE-----

# Issuer: CN=Izenpe.com O=IZENPE S.A.
# Subject: CN=Izenpe.com O=IZENPE S.A.
# Label: "Izenpe.com"
# Serial: 917563065490389241595536686991402621
# MD5 Fingerprint: a6:b0:cd:85:80:da:5c:50:34:a3:39:90:2f:55:67:73
# SHA1 Fingerprint: 2f:78:3d:25:52:18:a7:4a:65:39:71:b5:2c:a2:9c:45:15:6f:e9:19
# SHA256 Fingerprint: 25:30:cc:8e:98:32:15:02:ba:d9:6f:9b:1f:ba:1b:09:9e:2d:29:9e:0f:45:48:bb:91:4f:36:3b:c0:d4:53:1f
-----BEGIN CERTIFICATE-----
MIIF8TCCA9mgAwIBAgIQALC3WhZIX7/hy/WL1xnmfTANBgkqhkiG9w0BAQsFADA4
MQswCQYDVQQGEwJFUzEUMBIGA1UECgwLSVpFTlBFIFMuQS4xEzARBgNVBAMMCkl6
ZW5wZS5jb20wHhcNMDcxMjEzMTMwODI4WhcNMzcxMjEzMDgyNzI1WjA4MQswCQYD
VQQGEwJFUzEUMBIGA1UECgwLSVpFTlBFIFMuQS4xEzARBgNVBAMMCkl6ZW5wZS5j
b20wggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQDJ03rKDx6sp4boFmVq
scIbRTJxldn+EFvMr+eleQGPicPK8lVx93e+d5TzcqQsRNiekpsUOqHnJJAKClaO
xdgmlOHZSOEtPtoKct2jmRXagaKH9HtuJneJWK3W6wyyQXpzbm3benhB6QiIEn6H
LmYRY2xU+zydcsC8Lv/Ct90NduM61/e0aL6i9eOBbsFGb12N4E3GVFWJGjMxCrFX
uaOKmMPsOzTFlUFpfnXCPCDFYbpRR6AgkJOhkEvzTnyFRVSa0QUmQbC1TR0zvsQD
yCV8wXDbO/QJLVQnSKwv4cSsPsjLkkxTOTcj7NMB+eAJRE1NZMDhDVqHIrytG6P+
JrUV86f8hBnp7KGItERphIPzidF0BqnMC9bC3ieFUCbKF7jJeodWLBoBHmy+E60Q
rLUk9TiRodZL2vG70t5HtfG8gfZZa88ZU+mNFctKy6lvROUbQc/hhqfK0GqfvEyN
BjNaooXlkDWgYlwWTvDjovoDGrQscbNYLN57C9saD+veIR8GdwYDsMnvmfzAuU8L
hij+0rnq49qlw0dpEuDb8PYZi+17cNcC1u2HGCgsBCRMd+RIihrGO5rUD8r6ddIB
QFqNeb+Lz0vPqhbBleStTIo+F5HUsWLlguWABKQDfo2/2n+iD5dPDNMN+9fR5XJ+
HMh3/1uaD7euBUbl8agW7EekFwIDAQABo4H2MIHzMIGwBgNVHREEgagwgaWBD2lu
Zm9AaXplbnBlLmNvbaSBkTCBjjFHMEUGA1UECgw+SVpFTlBFIFMuQS4gLSBDSUYg
QTAxMzM3MjYwLVJNZXJjLlZpdG9yaWEtR2FzdGVpeiBUMTA1NSBGNjIgUzgxQzBB
BgNVBAkMOkF2ZGEgZGVsIE1lZGl0ZXJyYW5lbyBFdG9yYmlkZWEgMTQgLSAwMTAx
MCBWaXRvcmlhLUdhc3RlaXowDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMC
AQYwHQYDVR0OBBYEFB0cZQ6o8iV7tJHP5LGx5r1VdGwFMA0GCSqGSIb3DQEBCwUA
A4ICAQB4pgwWSp9MiDrAyw6lFn2fuUhfGI8NYjb2zRlrrKvV9pF9rnHzP7MOeIWb
laQnIUdCSnxIOvVFfLMMjlF4rJUT3sb9fbgakEyrkgPH7UIBzg/YsfqikuFgba56
awmqxinuaElnMIAkejEWOVt+8Rwu3WwJrfIxwYJOubv5vr8qhT/AQKM6WfxZSzwo
JNu0FXWuDYi6LnPAvViH5ULy617uHjAimcs30cQhbIHsvm0m5hzkQiCeR7Csg1lw
LDXWrzY0tM07+DKo7+N4ifuNRSzanLh+QBxh5z6ikixL8s36mLYp//Pye6kfLqCT
VyvehQP5aTfLnnhqBbTFMXiJ7HqnheG5ezzevh55hM6fcA5ZwjUukCox2eRFekGk
LhObNA5me0mrZJfQRsN5nXJQY6aYWwa9SG3YOYNw6DXwBdGqvOPbyALqfP2C2sJb
UjWumDqtujWTI6cfSN01RpiyEGjkpTHCClguGYEQyVB1/OpaFs4R1+7vUIgtYf8/
QnMFlEPVjjxOAToZpR9GTnfQXeWBIiGH/pR9hNiTrdZoQ0iy2+tzJOeRf1SktoA+
naM8THLCV8Sg1Mw4J87VBp6iSNnpn86CcDaTmjvfliHjWbcM2pE38P1ZWrOZyGls
QyYBNWNgVYkDOnXYukrZVP/u3oDYLdE41V4tC5h9Pmzb/CaIxw==
-----END CERTIFICATE-----

# Issuer: CN=Chambers of Commerce Root - 2008 O=AC Camerfirma S.A.
# Subject: CN=Chambers of Commerce Root - 2008 O=AC Camerfirma S.A.
# Label: "Chambers of Commerce Root - 2008"
# Serial: 11806822484801597146
# MD5 Fingerprint: 5e:80:9e:84:5a:0e:65:0b:17:02:f3:55:18:2a:3e:d7
# SHA1 Fingerprint: 78:6a:74:ac:76:ab:14:7f:9c:6a:30:50:ba:9e:a8:7e:fe:9a:ce:3c
# SHA256 Fingerprint: 06:3e:4a:fa:c4:91:df:d3:32:f3:08:9b:85:42:e9:46:17:d8:93:d7:fe:94:4e:10:a7:93:7e:e2:9d:96:93:c0
-----BEGIN CERTIFICATE-----
MIIHTzCCBTegAwIBAgIJAKPaQn6ksa7aMA0GCSqGSIb3DQEBBQUAMIGuMQswCQYD
VQQGEwJFVTFDMEEGA1UEBxM6TWFkcmlkIChzZWUgY3VycmVudCBhZGRyZXNzIGF0
IHd3dy5jYW1lcmZpcm1hLmNvbS9hZGRyZXNzKTESMBAGA1UEBRMJQTgyNzQzMjg3
MRswGQYDVQQKExJBQyBDYW1lcmZpcm1hIFMuQS4xKTAnBgNVBAMTIENoYW1iZXJz
IG9mIENvbW1lcmNlIFJvb3QgLSAyMDA4MB4XDTA4MDgwMTEyMjk1MFoXDTM4MDcz
MTEyMjk1MFowga4xCzAJBgNVBAYTAkVVMUMwQQYDVQQHEzpNYWRyaWQgKHNlZSBj
dXJyZW50IGFkZHJlc3MgYXQgd3d3LmNhbWVyZmlybWEuY29tL2FkZHJlc3MpMRIw
EAYDVQQFEwlBODI3NDMyODcxGzAZBgNVBAoTEkFDIENhbWVyZmlybWEgUy5BLjEp
MCcGA1UEAxMgQ2hhbWJlcnMgb2YgQ29tbWVyY2UgUm9vdCAtIDIwMDgwggIiMA0G
CSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQCvAMtwNyuAWko6bHiUfaN/Gh/2NdW9
28sNRHI+JrKQUrpjOyhYb6WzbZSm891kDFX29ufyIiKAXuFixrYp4YFs8r/lfTJq
VKAyGVn+H4vXPWCGhSRv4xGzdz4gljUha7MI2XAuZPeEklPWDrCQiorjh40G072Q
DuKZoRuGDtqaCrsLYVAGUvGef3bsyw/QHg3PmTA9HMRFEFis1tPo1+XqxQEHd9ZR
5gN/ikilTWh1uem8nk4ZcfUyS5xtYBkL+8ydddy/Js2Pk3g5eXNeJQ7KXOt3EgfL
ZEFHcpOrUMPrCXZkNNI5t3YRCQ12RcSprj1qr7V9ZS+UWBDsXHyvfuK2GNnQm05a
Sd+pZgvMPMZ4fKecHePOjlO+Bd5gD2vlGts/4+EhySnB8esHnFIbAURRPHsl18Tl
UlRdJQfKFiC4reRB7noI/plvg6aRArBsNlVq5331lubKgdaX8ZSD6e2wsWsSaR6s
+12pxZjptFtYer49okQ6Y1nUCyXeG0+95QGezdIp1Z8XGQpvvwyQ0wlf2eOKNcx5
Wk0ZN5K3xMGtr/R5JJqyAQuxr1yW84Ay+1w9mPGgP0revq+ULtlVmhduYJ1jbLhj
ya6BXBg14JC7vjxPNyK5fuvPnnchpj04gftI2jE9K+OJ9dC1vX7gUMQSibMjmhAx
hduub+84Mxh2EQIDAQABo4IBbDCCAWgwEgYDVR0TAQH/BAgwBgEB/wIBDDAdBgNV
HQ4EFgQU+SSsD7K1+HnA+mCIG8TZTQKeFxkwgeMGA1UdIwSB2zCB2IAU+SSsD7K1
+HnA+mCIG8TZTQKeFxmhgbSkgbEwga4xCzAJBgNVBAYTAkVVMUMwQQYDVQQHEzpN
YWRyaWQgKHNlZSBjdXJyZW50IGFkZHJlc3MgYXQgd3d3LmNhbWVyZmlybWEuY29t
L2FkZHJlc3MpMRIwEAYDVQQFEwlBODI3NDMyODcxGzAZBgNVBAoTEkFDIENhbWVy
ZmlybWEgUy5BLjEpMCcGA1UEAxMgQ2hhbWJlcnMgb2YgQ29tbWVyY2UgUm9vdCAt
IDIwMDiCCQCj2kJ+pLGu2jAOBgNVHQ8BAf8EBAMCAQYwPQYDVR0gBDYwNDAyBgRV
HSAAMCowKAYIKwYBBQUHAgEWHGh0dHA6Ly9wb2xpY3kuY2FtZXJmaXJtYS5jb20w
DQYJKoZIhvcNAQEFBQADggIBAJASryI1wqM58C7e6bXpeHxIvj99RZJe6dqxGfwW
PJ+0W2aeaufDuV2I6A+tzyMP3iU6XsxPpcG1Lawk0lgH3qLPaYRgM+gQDROpI9CF
5Y57pp49chNyM/WqfcZjHwj0/gF/JM8rLFQJ3uIrbZLGOU8W6jx+ekbURWpGqOt1
glanq6B8aBMz9p0w8G8nOSQjKpD9kCk18pPfNKXG9/jvjA9iSnyu0/VU+I22mlaH
FoI6M6taIgj3grrqLuBHmrS1RaMFO9ncLkVAO+rcf+g769HsJtg1pDDFOqxXnrN2
pSB7+R5KBWIBpih1YJeSDW4+TTdDDZIVnBgizVGZoCkaPF+KMjNbMMeJL0eYD6MD
xvbxrN8y8NmBGuScvfaAFPDRLLmF9dijscilIeUcE5fuDr3fKanvNFNb0+RqE4QG
tjICxFKuItLcsiFCGtpA8CnJ7AoMXOLQusxI0zcKzBIKinmwPQN/aUv0NCB9szTq
jktk9T79syNnFQ0EuPAtwQlRPLJsFfClI9eDdOTlLsn+mCdCxqvGnrDQWzilm1De
fhiYtUU79nm06PcaewaD+9CL2rvHvRirCG88gGtAPxkZumWK5r7VXNM21+9AUiRg
OGcEMeyP84LG3rlV8zsxkVrctQgVrXYlCg17LofiDKYGvCYQbTed7N14jHyAxfDZ
d0jQ
-----END CERTIFICATE-----

# Issuer: CN=Global Chambersign Root - 2008 O=AC Camerfirma S.A.
# Subject: CN=Global Chambersign Root - 2008 O=AC Camerfirma S.A.
# Label: "Global Chambersign Root - 2008"
# Serial: 14541511773111788494
# MD5 Fingerprint: 9e:80:ff:78:01:0c:2e:c1:36:bd:fe:96:90:6e:08:f3
# SHA1 Fingerprint: 4a:bd:ee:ec:95:0d:35:9c:89:ae:c7:52:a1:2c:5b:29:f6:d6:aa:0c
# SHA256 Fingerprint: 13:63:35:43:93:34:a7:69:80:16:a0:d3:24:de:72:28:4e:07:9d:7b:52:20:bb:8f:bd:74:78:16:ee:be:ba:ca
-----BEGIN CERTIFICATE-----
MIIHSTCCBTGgAwIBAgIJAMnN0+nVfSPOMA0GCSqGSIb3DQEBBQUAMIGsMQswCQYD
VQQGEwJFVTFDMEEGA1UEBxM6TWFkcmlkIChzZWUgY3VycmVudCBhZGRyZXNzIGF0
IHd3dy5jYW1lcmZpcm1hLmNvbS9hZGRyZXNzKTESMBAGA1UEBRMJQTgyNzQzMjg3
MRswGQYDVQQKExJBQyBDYW1lcmZpcm1hIFMuQS4xJzAlBgNVBAMTHkdsb2JhbCBD
aGFtYmVyc2lnbiBSb290IC0gMjAwODAeFw0wODA4MDExMjMxNDBaFw0zODA3MzEx
MjMxNDBaMIGsMQswCQYDVQQGEwJFVTFDMEEGA1UEBxM6TWFkcmlkIChzZWUgY3Vy
cmVudCBhZGRyZXNzIGF0IHd3dy5jYW1lcmZpcm1hLmNvbS9hZGRyZXNzKTESMBAG
A1UEBRMJQTgyNzQzMjg3MRswGQYDVQQKExJBQyBDYW1lcmZpcm1hIFMuQS4xJzAl
BgNVBAMTHkdsb2JhbCBDaGFtYmVyc2lnbiBSb290IC0gMjAwODCCAiIwDQYJKoZI
hvcNAQEBBQADggIPADCCAgoCggIBAMDfVtPkOpt2RbQT2//BthmLN0EYlVJH6xed
KYiONWwGMi5HYvNJBL99RDaxccy9Wglz1dmFRP+RVyXfXjaOcNFccUMd2drvXNL7
G706tcuto8xEpw2uIRU/uXpbknXYpBI4iRmKt4DS4jJvVpyR1ogQC7N0ZJJ0YPP2
zxhPYLIj0Mc7zmFLmY/CDNBAspjcDahOo7kKrmCgrUVSY7pmvWjg+b4aqIG7HkF4
ddPB/gBVsIdU6CeQNR1MM62X/JcumIS/LMmjv9GYERTtY/jKmIhYF5ntRQOXfjyG
HoiMvvKRhI9lNNgATH23MRdaKXoKGCQwoze1eqkBfSbW+Q6OWfH9GzO1KTsXO0G2
Id3UwD2ln58fQ1DJu7xsepeY7s2MH/ucUa6LcL0nn3HAa6x9kGbo1106DbDVwo3V
yJ2dwW3Q0L9R5OP4wzg2rtandeavhENdk5IMagfeOx2YItaswTXbo6Al/3K1dh3e
beksZixShNBFks4c5eUzHdwHU1SjqoI7mjcv3N2gZOnm3b2u/GSFHTynyQbehP9r
6GsaPMWis0L7iwk+XwhSx2LE1AVxv8Rk5Pihg+g+EpuoHtQ2TS9x9o0o9oOpE9Jh
wZG7SMA0j0GMS0zbaRL/UJScIINZc+18ofLx/d33SdNDWKBWY8o9PeU1VlnpDsog
zCtLkykPAgMBAAGjggFqMIIBZjASBgNVHRMBAf8ECDAGAQH/AgEMMB0GA1UdDgQW
BBS5CcqcHtvTbDprru1U8VuTBjUuXjCB4QYDVR0jBIHZMIHWgBS5CcqcHtvTbDpr
ru1U8VuTBjUuXqGBsqSBrzCBrDELMAkGA1UEBhMCRVUxQzBBBgNVBAcTOk1hZHJp
ZCAoc2VlIGN1cnJlbnQgYWRkcmVzcyBhdCB3d3cuY2FtZXJmaXJtYS5jb20vYWRk
cmVzcykxEjAQBgNVBAUTCUE4Mjc0MzI4NzEbMBkGA1UEChMSQUMgQ2FtZXJmaXJt
YSBTLkEuMScwJQYDVQQDEx5HbG9iYWwgQ2hhbWJlcnNpZ24gUm9vdCAtIDIwMDiC
CQDJzdPp1X0jzjAOBgNVHQ8BAf8EBAMCAQYwPQYDVR0gBDYwNDAyBgRVHSAAMCow
KAYIKwYBBQUHAgEWHGh0dHA6Ly9wb2xpY3kuY2FtZXJmaXJtYS5jb20wDQYJKoZI
hvcNAQEFBQADggIBAICIf3DekijZBZRG/5BXqfEv3xoNa/p8DhxJJHkn2EaqbylZ
UohwEurdPfWbU1Rv4WCiqAm57OtZfMY18dwY6fFn5a+6ReAJ3spED8IXDneRRXoz
X1+WLGiLwUePmJs9wOzL9dWCkoQ10b42OFZyMVtHLaoXpGNR6woBrX/sdZ7LoR/x
fxKxueRkf2fWIyr0uDldmOghp+G9PUIadJpwr2hsUF1Jz//7Dl3mLEfXgTpZALVz
a2Mg9jFFCDkO9HB+QHBaP9BrQql0PSgvAm11cpUJjUhjxsYjV5KTXjXBjfkK9yyd
Yhz2rXzdpjEetrHHfoUm+qRqtdpjMNHvkzeyZi99Bffnt0uYlDXA2TopwZ2yUDMd
SqlapskD7+3056huirRXhOukP9DuqqqHW2Pok+JrqNS4cnhrG+055F3Lm6qH1U9O
AP7Zap88MQ8oAgF9mOinsKJknnn4SPIVqczmyETrP3iZ8ntxPjzxmKfFGBI/5rso
M0LpRQp8bfKGeS/Fghl9CYl8slR2iK7ewfPM4W7bMdaTrpmg7yVqc5iJWzouE4ge
v8CSlDQb4ye3ix5vQv/n6TebUB0tovkC7stYWDpxvGjjqsGvHCgfotwjZT+B6q6Z
09gwzxMNTxXJhLynSC34MCN32EZLeW32jO06f2ARePTpm67VVMB0gNELQp/B
-----END CERTIFICATE-----

# Issuer: CN=Go Daddy Root Certificate Authority - G2 O=GoDaddy.com, Inc.
# Subject: CN=Go Daddy Root Certificate Authority - G2 O=GoDaddy.com, Inc.
# Label: "Go Daddy Root Certificate Authority - G2"
# Serial: 0
# MD5 Fingerprint: 80:3a:bc:22:c1:e6:fb:8d:9b:3b:27:4a:32:1b:9a:01
# SHA1 Fingerprint: 47:be:ab:c9:22:ea:e8:0e:78:78:34:62:a7:9f:45:c2:54:fd:e6:8b
# SHA256 Fingerprint: 45:14:0b:32:47:eb:9c:c8:c5:b4:f0:d7:b5:30:91:f7:32:92:08:9e:6e:5a:63:e2:74:9d:d3:ac:a9:19:8e:da
-----BEGIN CERTIFICATE-----
MIIDxTCCAq2gAwIBAgIBADANBgkqhkiG9w0BAQsFADCBgzELMAkGA1UEBhMCVVMx
EDAOBgNVBAgTB0FyaXpvbmExEzARBgNVBAcTClNjb3R0c2RhbGUxGjAYBgNVBAoT
EUdvRGFkZHkuY29tLCBJbmMuMTEwLwYDVQQDEyhHbyBEYWRkeSBSb290IENlcnRp
ZmljYXRlIEF1dGhvcml0eSAtIEcyMB4XDTA5MDkwMTAwMDAwMFoXDTM3MTIzMTIz
NTk1OVowgYMxCzAJBgNVBAYTAlVTMRAwDgYDVQQIEwdBcml6b25hMRMwEQYDVQQH
EwpTY290dHNkYWxlMRowGAYDVQQKExFHb0RhZGR5LmNvbSwgSW5jLjExMC8GA1UE
AxMoR28gRGFkZHkgUm9vdCBDZXJ0aWZpY2F0ZSBBdXRob3JpdHkgLSBHMjCCASIw
DQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAL9xYgjx+lk09xvJGKP3gElY6SKD
E6bFIEMBO4Tx5oVJnyfq9oQbTqC023CYxzIBsQU+B07u9PpPL1kwIuerGVZr4oAH
/PMWdYA5UXvl+TW2dE6pjYIT5LY/qQOD+qK+ihVqf94Lw7YZFAXK6sOoBJQ7Rnwy
DfMAZiLIjWltNowRGLfTshxgtDj6AozO091GB94KPutdfMh8+7ArU6SSYmlRJQVh
GkSBjCypQ5Yj36w6gZoOKcUcqeldHraenjAKOc7xiID7S13MMuyFYkMlNAJWJwGR
tDtwKj9useiciAF9n9T521NtYJ2/LOdYq7hfRvzOxBsDPAnrSTFcaUaz4EcCAwEA
AaNCMEAwDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAQYwHQYDVR0OBBYE
FDqahQcQZyi27/a9BUFuIMGU2g/eMA0GCSqGSIb3DQEBCwUAA4IBAQCZ21151fmX
WWcDYfF+OwYxdS2hII5PZYe096acvNjpL9DbWu7PdIxztDhC2gV7+AJ1uP2lsdeu
9tfeE8tTEH6KRtGX+rcuKxGrkLAngPnon1rpN5+r5N9ss4UXnT3ZJE95kTXWXwTr
gIOrmgIttRD02JDHBHNA7XIloKmf7J6raBKZV8aPEjoJpL1E/QYVN8Gb5DKj7Tjo
2GTzLH4U/ALqn83/B2gX2yKQOC16jdFU8WnjXzPKej17CuPKf1855eJ1usV2GDPO
LPAvTK33sefOT6jEm0pUBsV/fdUID+Ic/n4XuKxe9tQWskMJDE32p2u0mYRlynqI
4uJEvlz36hz1
-----END CERTIFICATE-----

# Issuer: CN=Starfield Root Certificate Authority - G2 O=Starfield Technologies, Inc.
# Subject: CN=Starfield Root Certificate Authority - G2 O=Starfield Technologies, Inc.
# Label: "Starfield Root Certificate Authority - G2"
# Serial: 0
# MD5 Fingerprint: d6:39:81:c6:52:7e:96:69:fc:fc:ca:66:ed:05:f2:96
# SHA1 Fingerprint: b5:1c:06:7c:ee:2b:0c:3d:f8:55:ab:2d:92:f4:fe:39:d4:e7:0f:0e
# SHA256 Fingerprint: 2c:e1:cb:0b:f9:d2:f9:e1:02:99:3f:be:21:51:52:c3:b2:dd:0c:ab:de:1c:68:e5:31:9b:83:91:54:db:b7:f5
-----BEGIN CERTIFICATE-----
MIID3TCCAsWgAwIBAgIBADANBgkqhkiG9w0BAQsFADCBjzELMAkGA1UEBhMCVVMx
EDAOBgNVBAgTB0FyaXpvbmExEzARBgNVBAcTClNjb3R0c2RhbGUxJTAjBgNVBAoT
HFN0YXJmaWVsZCBUZWNobm9sb2dpZXMsIEluYy4xMjAwBgNVBAMTKVN0YXJmaWVs
ZCBSb290IENlcnRpZmljYXRlIEF1dGhvcml0eSAtIEcyMB4XDTA5MDkwMTAwMDAw
MFoXDTM3MTIzMTIzNTk1OVowgY8xCzAJBgNVBAYTAlVTMRAwDgYDVQQIEwdBcml6
b25hMRMwEQYDVQQHEwpTY290dHNkYWxlMSUwIwYDVQQKExxTdGFyZmllbGQgVGVj
aG5vbG9naWVzLCBJbmMuMTIwMAYDVQQDEylTdGFyZmllbGQgUm9vdCBDZXJ0aWZp
Y2F0ZSBBdXRob3JpdHkgLSBHMjCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoC
ggEBAL3twQP89o/8ArFvW59I2Z154qK3A2FWGMNHttfKPTUuiUP3oWmb3ooa/RMg
nLRJdzIpVv257IzdIvpy3Cdhl+72WoTsbhm5iSzchFvVdPtrX8WJpRBSiUZV9Lh1
HOZ/5FSuS/hVclcCGfgXcVnrHigHdMWdSL5stPSksPNkN3mSwOxGXn/hbVNMYq/N
Hwtjuzqd+/x5AJhhdM8mgkBj87JyahkNmcrUDnXMN/uLicFZ8WJ/X7NfZTD4p7dN
dloedl40wOiWVpmKs/B/pM293DIxfJHP4F8R+GuqSVzRmZTRouNjWwl2tVZi4Ut0
HZbUJtQIBFnQmA4O5t78w+wfkPECAwEAAaNCMEAwDwYDVR0TAQH/BAUwAwEB/zAO
BgNVHQ8BAf8EBAMCAQYwHQYDVR0OBBYEFHwMMh+n2TB/xH1oo2Kooc6rB1snMA0G
CSqGSIb3DQEBCwUAA4IBAQARWfolTwNvlJk7mh+ChTnUdgWUXuEok21iXQnCoKjU
sHU48TRqneSfioYmUeYs0cYtbpUgSpIB7LiKZ3sx4mcujJUDJi5DnUox9g61DLu3
4jd/IroAow57UvtruzvE03lRTs2Q9GcHGcg8RnoNAX3FWOdt5oUwF5okxBDgBPfg
8n/Uqgr/Qh037ZTlZFkSIHc40zI+OIF1lnP6aI+xy84fxez6nH7PfrHxBy22/L/K
pL/QlwVKvOoYKAKQvVR4CSFx09F9HdkWsKlhPdAKACL8x3vLCWRFCztAgfd9fDL1
mMpYjn0q7pBZc2T5NnReJaH1ZgUufzkVqSr7UIuOhWn0
-----END CERTIFICATE-----

# Issuer: CN=Starfield Services Root Certificate Authority - G2 O=Starfield Technologies, Inc.
# Subject: CN=Starfield Services Root Certificate Authority - G2 O=Starfield Technologies, Inc.
# Label: "Starfield Services Root Certificate Authority - G2"
# Serial: 0
# MD5 Fingerprint: 17:35:74:af:7b:61:1c:eb:f4:f9:3c:e2:ee:40:f9:a2
# SHA1 Fingerprint: 92:5a:8f:8d:2c:6d:04:e0:66:5f:59:6a:ff:22:d8:63:e8:25:6f:3f
# SHA256 Fingerprint: 56:8d:69:05:a2:c8:87:08:a4:b3:02:51:90:ed:cf:ed:b1:97:4a:60:6a:13:c6:e5:29:0f:cb:2a:e6:3e:da:b5
-----BEGIN CERTIFICATE-----
MIID7zCCAtegAwIBAgIBADANBgkqhkiG9w0BAQsFADCBmDELMAkGA1UEBhMCVVMx
EDAOBgNVBAgTB0FyaXpvbmExEzARBgNVBAcTClNjb3R0c2RhbGUxJTAjBgNVBAoT
HFN0YXJmaWVsZCBUZWNobm9sb2dpZXMsIEluYy4xOzA5BgNVBAMTMlN0YXJmaWVs
ZCBTZXJ2aWNlcyBSb290IENlcnRpZmljYXRlIEF1dGhvcml0eSAtIEcyMB4XDTA5
MDkwMTAwMDAwMFoXDTM3MTIzMTIzNTk1OVowgZgxCzAJBgNVBAYTAlVTMRAwDgYD
VQQIEwdBcml6b25hMRMwEQYDVQQHEwpTY290dHNkYWxlMSUwIwYDVQQKExxTdGFy
ZmllbGQgVGVjaG5vbG9naWVzLCBJbmMuMTswOQYDVQQDEzJTdGFyZmllbGQgU2Vy
dmljZXMgUm9vdCBDZXJ0aWZpY2F0ZSBBdXRob3JpdHkgLSBHMjCCASIwDQYJKoZI
hvcNAQEBBQADggEPADCCAQoCggEBANUMOsQq+U7i9b4Zl1+OiFOxHz/Lz58gE20p
OsgPfTz3a3Y4Y9k2YKibXlwAgLIvWX/2h/klQ4bnaRtSmpDhcePYLQ1Ob/bISdm2
8xpWriu2dBTrz/sm4xq6HZYuajtYlIlHVv8loJNwU4PahHQUw2eeBGg6345AWh1K
Ts9DkTvnVtYAcMtS7nt9rjrnvDH5RfbCYM8TWQIrgMw0R9+53pBlbQLPLJGmpufe
hRhJfGZOozptqbXuNC66DQO4M99H67FrjSXZm86B0UVGMpZwh94CDklDhbZsc7tk
6mFBrMnUVN+HL8cisibMn1lUaJ/8viovxFUcdUBgF4UCVTmLfwUCAwEAAaNCMEAw
DwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAQYwHQYDVR0OBBYEFJxfAN+q
AdcwKziIorhtSpzyEZGDMA0GCSqGSIb3DQEBCwUAA4IBAQBLNqaEd2ndOxmfZyMI
bw5hyf2E3F/YNoHN2BtBLZ9g3ccaaNnRbobhiCPPE95Dz+I0swSdHynVv/heyNXB
ve6SbzJ08pGCL72CQnqtKrcgfU28elUSwhXqvfdqlS5sdJ/PHLTyxQGjhdByPq1z
qwubdQxtRbeOlKyWN7Wg0I8VRw7j6IPdj/3vQQF3zCepYoUz8jcI73HPdwbeyBkd
iEDPfUYd/x7H4c7/I9vG+o1VTqkC50cRRj70/b17KSa7qWFiNyi2LSr2EIZkyXCn
0q23KXB56jzaYyWf/Wi3MOxw+3WKt21gZ7IeyLnp2KhvAotnDU0mV3HaIPzBSlCN
sSi6
-----END CERTIFICATE-----

# Issuer: CN=AffirmTrust Commercial O=AffirmTrust
# Subject: CN=AffirmTrust Commercial O=AffirmTrust
# Label: "AffirmTrust Commercial"
# Serial: 8608355977964138876
# MD5 Fingerprint: 82:92:ba:5b:ef:cd:8a:6f:a6:3d:55:f9:84:f6:d6:b7
# SHA1 Fingerprint: f9:b5:b6:32:45:5f:9c:be:ec:57:5f:80:dc:e9:6e:2c:c7:b2:78:b7
# SHA256 Fingerprint: 03:76:ab:1d:54:c5:f9:80:3c:e4:b2:e2:01:a0:ee:7e:ef:7b:57:b6:36:e8:a9:3c:9b:8d:48:60:c9:6f:5f:a7
-----BEGIN CERTIFICATE-----
MIIDTDCCAjSgAwIBAgIId3cGJyapsXwwDQYJKoZIhvcNAQELBQAwRDELMAkGA1UE
BhMCVVMxFDASBgNVBAoMC0FmZmlybVRydXN0MR8wHQYDVQQDDBZBZmZpcm1UcnVz
dCBDb21tZXJjaWFsMB4XDTEwMDEyOTE0MDYwNloXDTMwMTIzMTE0MDYwNlowRDEL
MAkGA1UEBhMCVVMxFDASBgNVBAoMC0FmZmlybVRydXN0MR8wHQYDVQQDDBZBZmZp
cm1UcnVzdCBDb21tZXJjaWFsMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKC
AQEA9htPZwcroRX1BiLLHwGy43NFBkRJLLtJJRTWzsO3qyxPxkEylFf6EqdbDuKP
Hx6GGaeqtS25Xw2Kwq+FNXkyLbscYjfysVtKPcrNcV/pQr6U6Mje+SJIZMblq8Yr
ba0F8PrVC8+a5fBQpIs7R6UjW3p6+DM/uO+Zl+MgwdYoic+U+7lF7eNAFxHUdPAL
MeIrJmqbTFeurCA+ukV6BfO9m2kVrn1OIGPENXY6BwLJN/3HR+7o8XYdcxXyl6S1
yHp52UKqK39c/s4mT6NmgTWvRLpUHhwwMmWd5jyTXlBOeuM61G7MGvv50jeuJCqr
VwMiKA1JdX+3KNp1v47j3A55MQIDAQABo0IwQDAdBgNVHQ4EFgQUnZPGU4teyq8/
nx4P5ZmVvCT2lI8wDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAQYwDQYJ
KoZIhvcNAQELBQADggEBAFis9AQOzcAN/wr91LoWXym9e2iZWEnStB03TX8nfUYG
XUPGhi4+c7ImfU+TqbbEKpqrIZcUsd6M06uJFdhrJNTxFq7YpFzUf1GO7RgBsZNj
vbz4YYCanrHOQnDiqX0GJX0nof5v7LMeJNrjS1UaADs1tDvZ110w/YETifLCBivt
Z8SOyUOyXGsViQK8YvxO8rUzqrJv0wqiUOP2O+guRMLbZjipM1ZI8W0bM40NjD9g
N53Tym1+NH4Nn3J2ixufcv1SNUFFApYvHLKac0khsUlHRUe072o0EclNmsxZt9YC
nlpOZbWUrhvfKbAW8b8Angc6F2S1BLUjIZkKlTuXfO8=
-----END CERTIFICATE-----

# Issuer: CN=AffirmTrust Networking O=AffirmTrust
# Subject: CN=AffirmTrust Networking O=AffirmTrust
# Label: "AffirmTrust Networking"
# Serial: 8957382827206547757
# MD5 Fingerprint: 42:65:ca:be:01:9a:9a:4c:a9:8c:41:49:cd:c0:d5:7f
# SHA1 Fingerprint: 29:36:21:02:8b:20:ed:02:f5:66:c5:32:d1:d6:ed:90:9f:45:00:2f
# SHA256 Fingerprint: 0a:81:ec:5a:92:97:77:f1:45:90:4a:f3:8d:5d:50:9f:66:b5:e2:c5:8f:cd:b5:31:05:8b:0e:17:f3:f0:b4:1b
-----BEGIN CERTIFICATE-----
MIIDTDCCAjSgAwIBAgIIfE8EORzUmS0wDQYJKoZIhvcNAQEFBQAwRDELMAkGA1UE
BhMCVVMxFDASBgNVBAoMC0FmZmlybVRydXN0MR8wHQYDVQQDDBZBZmZpcm1UcnVz
dCBOZXR3b3JraW5nMB4XDTEwMDEyOTE0MDgyNFoXDTMwMTIzMTE0MDgyNFowRDEL
MAkGA1UEBhMCVVMxFDASBgNVBAoMC0FmZmlybVRydXN0MR8wHQYDVQQDDBZBZmZp
cm1UcnVzdCBOZXR3b3JraW5nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKC
AQEAtITMMxcua5Rsa2FSoOujz3mUTOWUgJnLVWREZY9nZOIG41w3SfYvm4SEHi3y
YJ0wTsyEheIszx6e/jarM3c1RNg1lho9Nuh6DtjVR6FqaYvZ/Ls6rnla1fTWcbua
kCNrmreIdIcMHl+5ni36q1Mr3Lt2PpNMCAiMHqIjHNRqrSK6mQEubWXLviRmVSRL
QESxG9fhwoXA3hA/Pe24/PHxI1Pcv2WXb9n5QHGNfb2V1M6+oF4nI979ptAmDgAp
6zxG8D1gvz9Q0twmQVGeFDdCBKNwV6gbh+0t+nvujArjqWaJGctB+d1ENmHP4ndG
yH329JKBNv3bNPFyfvMMFr20FQIDAQABo0IwQDAdBgNVHQ4EFgQUBx/S55zawm6i
QLSwelAQUHTEyL0wDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAQYwDQYJ
KoZIhvcNAQEFBQADggEBAIlXshZ6qML91tmbmzTCnLQyFE2npN/svqe++EPbkTfO
tDIuUFUaNU52Q3Eg75N3ThVwLofDwR1t3Mu1J9QsVtFSUzpE0nPIxBsFZVpikpzu
QY0x2+c06lkh1QF612S4ZDnNye2v7UsDSKegmQGA3GWjNq5lWUhPgkvIZfFXHeVZ
Lgo/bNjR9eUJtGxUAArgFU2HdW23WJZa3W3SAKD0m0i+wzekujbgfIeFlxoVot4u
olu9rxj5kFDNcFn4J2dHy8egBzp90SxdbBk6ZrV9/ZFvgrG+CJPbFEfxojfHRZ48
x3evZKiT3/Zpg4Jg8klCNO1aAFSFHBY2kgxc+qatv9s=
-----END CERTIFICATE-----

# Issuer: CN=AffirmTrust Premium O=AffirmTrust
# Subject: CN=AffirmTrust Premium O=AffirmTrust
# Label: "AffirmTrust Premium"
# Serial: 7893706540734352110
# MD5 Fingerprint: c4:5d:0e:48:b6:ac:28:30:4e:0a:bc:f9:38:16:87:57
# SHA1 Fingerprint: d8:a6:33:2c:e0:03:6f:b1:85:f6:63:4f:7d:6a:06:65:26:32:28:27
# SHA256 Fingerprint: 70:a7:3f:7f:37:6b:60:07:42:48:90:45:34:b1:14:82:d5:bf:0e:69:8e:cc:49:8d:f5:25:77:eb:f2:e9:3b:9a
-----BEGIN CERTIFICATE-----
MIIFRjCCAy6gAwIBAgIIbYwURrGmCu4wDQYJKoZIhvcNAQEMBQAwQTELMAkGA1UE
BhMCVVMxFDASBgNVBAoMC0FmZmlybVRydXN0MRwwGgYDVQQDDBNBZmZpcm1UcnVz
dCBQcmVtaXVtMB4XDTEwMDEyOTE0MTAzNloXDTQwMTIzMTE0MTAzNlowQTELMAkG
A1UEBhMCVVMxFDASBgNVBAoMC0FmZmlybVRydXN0MRwwGgYDVQQDDBNBZmZpcm1U
cnVzdCBQcmVtaXVtMIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAxBLf
qV/+Qd3d9Z+K4/as4Tx4mrzY8H96oDMq3I0gW64tb+eT2TZwamjPjlGjhVtnBKAQ
JG9dKILBl1fYSCkTtuG+kU3fhQxTGJoeJKJPj/CihQvL9Cl/0qRY7iZNyaqoe5rZ
+jjeRFcV5fiMyNlI4g0WJx0eyIOFJbe6qlVBzAMiSy2RjYvmia9mx+n/K+k8rNrS
s8PhaJyJ+HoAVt70VZVs+7pk3WKL3wt3MutizCaam7uqYoNMtAZ6MMgpv+0GTZe5
HMQxK9VfvFMSF5yZVylmd2EhMQcuJUmdGPLu8ytxjLW6OQdJd/zvLpKQBY0tL3d7
70O/Nbua2Plzpyzy0FfuKE4mX4+QaAkvuPjcBukumj5Rp9EixAqnOEhss/n/fauG
V+O61oV4d7pD6kh/9ti+I20ev9E2bFhc8e6kGVQa9QPSdubhjL08s9NIS+LI+H+S
qHZGnEJlPqQewQcDWkYtuJfzt9WyVSHvutxMAJf7FJUnM7/oQ0dG0giZFmA7mn7S
5u046uwBHjxIVkkJx0w3AJ6IDsBz4W9m6XJHMD4Q5QsDyZpCAGzFlH5hxIrff4Ia
C1nEWTJ3s7xgaVY5/bQGeyzWZDbZvUjthB9+pSKPKrhC9IK31FOQeE4tGv2Bb0TX
OwF0lkLgAOIua+rF7nKsu7/+6qqo+Nz2snmKtmcCAwEAAaNCMEAwHQYDVR0OBBYE
FJ3AZ6YMItkm9UWrpmVSESfYRaxjMA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/
BAQDAgEGMA0GCSqGSIb3DQEBDAUAA4ICAQCzV00QYk465KzquByvMiPIs0laUZx2
KI15qldGF9X1Uva3ROgIRL8YhNILgM3FEv0AVQVhh0HctSSePMTYyPtwni94loMg
Nt58D2kTiKV1NpgIpsbfrM7jWNa3Pt668+s0QNiigfV4Py/VpfzZotReBA4Xrf5B
8OWycvpEgjNC6C1Y91aMYj+6QrCcDFx+LmUmXFNPALJ4fqENmS2NuB2OosSw/WDQ
MKSOyARiqcTtNd56l+0OOF6SL5Nwpamcb6d9Ex1+xghIsV5n61EIJenmJWtSKZGc
0jlzCFfemQa0W50QBuHCAKi4HEoCChTQwUHK+4w1IX2COPKpVJEZNZOUbWo6xbLQ
u4mGk+ibyQ86p3q4ofB4Rvr8Ny/lioTz3/4E2aFooC8k4gmVBtWVyuEklut89pMF
u+1z6S3RdTnX5yTb2E5fQ4+e0BQ5v1VwSJlXMbSc7kqYA5YwH2AG7hsj/oFgIxpH
YoWlzBk0gG+zrBrjn/B7SK3VAdlntqlyk+otZrWyuOQ9PLLvTIzq6we/qzWaVYa8
GKa1qF60g2xraUDTn9zxw2lrueFtCfTxqlB2Cnp9ehehVZZCmTEJ3WARjQUwfuaO
RtGdFNrHF+QFlozEJLUbzxQHskD4o55BhrwE0GuWyCqANP2/7waj3VjFhT0+j/6e
KeC2uAloGRwYQw==
-----END CERTIFICATE-----

# Issuer: CN=AffirmTrust Premium ECC O=AffirmTrust
# Subject: CN=AffirmTrust Premium ECC O=AffirmTrust
# Label: "AffirmTrust Premium ECC"
# Serial: 8401224907861490260
# MD5 Fingerprint: 64:b0:09:55:cf:b1:d5:99:e2:be:13:ab:a6:5d:ea:4d
# SHA1 Fingerprint: b8:23:6b:00:2f:1d:16:86:53:01:55:6c:11:a4:37:ca:eb:ff:c3:bb
# SHA256 Fingerprint: bd:71:fd:f6:da:97:e4:cf:62:d1:64:7a:dd:25:81:b0:7d:79:ad:f8:39:7e:b4:ec:ba:9c:5e:84:88:82:14:23
-----BEGIN CERTIFICATE-----
MIIB/jCCAYWgAwIBAgIIdJclisc/elQwCgYIKoZIzj0EAwMwRTELMAkGA1UEBhMC
VVMxFDASBgNVBAoMC0FmZmlybVRydXN0MSAwHgYDVQQDDBdBZmZpcm1UcnVzdCBQ
cmVtaXVtIEVDQzAeFw0xMDAxMjkxNDIwMjRaFw00MDEyMzExNDIwMjRaMEUxCzAJ
BgNVBAYTAlVTMRQwEgYDVQQKDAtBZmZpcm1UcnVzdDEgMB4GA1UEAwwXQWZmaXJt
VHJ1c3QgUHJlbWl1bSBFQ0MwdjAQBgcqhkjOPQIBBgUrgQQAIgNiAAQNMF4bFZ0D
0KF5Nbc6PJJ6yhUczWLznCZcBz3lVPqj1swS6vQUX+iOGasvLkjmrBhDeKzQN8O9
ss0s5kfiGuZjuD0uL3jET9v0D6RoTFVya5UdThhClXjMNzyR4ptlKymjQjBAMB0G
A1UdDgQWBBSaryl6wBE1NSZRMADDav5A1a7WPDAPBgNVHRMBAf8EBTADAQH/MA4G
A1UdDwEB/wQEAwIBBjAKBggqhkjOPQQDAwNnADBkAjAXCfOHiFBar8jAQr9HX/Vs
aobgxCd05DhT1wV/GzTjxi+zygk8N53X57hG8f2h4nECMEJZh0PUUd+60wkyWs6I
flc9nF9Ca/UHLbXwgpP5WW+uZPpY5Yse42O+tYHNbwKMeQ==
-----END CERTIFICATE-----

# Issuer: CN=Certum Trusted Network CA O=Unizeto Technologies S.A. OU=Certum Certification Authority
# Subject: CN=Certum Trusted Network CA O=Unizeto Technologies S.A. OU=Certum Certification Authority
# Label: "Certum Trusted Network CA"
# Serial: 279744
# MD5 Fingerprint: d5:e9:81:40:c5:18:69:fc:46:2c:89:75:62:0f:aa:78
# SHA1 Fingerprint: 07:e0:32:e0:20:b7:2c:3f:19:2f:06:28:a2:59:3a:19:a7:0f:06:9e
# SHA256 Fingerprint: 5c:58:46:8d:55:f5:8e:49:7e:74:39:82:d2:b5:00:10:b6:d1:65:37:4a:cf:83:a7:d4:a3:2d:b7:68:c4:40:8e
-----BEGIN CERTIFICATE-----
MIIDuzCCAqOgAwIBAgIDBETAMA0GCSqGSIb3DQEBBQUAMH4xCzAJBgNVBAYTAlBM
MSIwIAYDVQQKExlVbml6ZXRvIFRlY2hub2xvZ2llcyBTLkEuMScwJQYDVQQLEx5D
ZXJ0dW0gQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkxIjAgBgNVBAMTGUNlcnR1bSBU
cnVzdGVkIE5ldHdvcmsgQ0EwHhcNMDgxMDIyMTIwNzM3WhcNMjkxMjMxMTIwNzM3
WjB+MQswCQYDVQQGEwJQTDEiMCAGA1UEChMZVW5pemV0byBUZWNobm9sb2dpZXMg
Uy5BLjEnMCUGA1UECxMeQ2VydHVtIENlcnRpZmljYXRpb24gQXV0aG9yaXR5MSIw
IAYDVQQDExlDZXJ0dW0gVHJ1c3RlZCBOZXR3b3JrIENBMIIBIjANBgkqhkiG9w0B
AQEFAAOCAQ8AMIIBCgKCAQEA4/t9o3K6wvDJFIf1awFO4W5AB7ptJ11/91sts1rH
UV+rpDKmYYe2bg+G0jACl/jXaVehGDldamR5xgFZrDwxSjh80gTSSyjoIF87B6LM
TXPb865Px1bVWqeWifrzq2jUI4ZZJ88JJ7ysbnKDHDBy3+Ci6dLhdHUZvSqeexVU
BBvXQzmtVSjF4hq79MDkrjhJM8x2hZ85RdKknvISjFH4fOQtf/WsX+sWn7Et0brM
kUJ3TCXJkDhv2/DM+44el1k+1WBO5gUo7Ul5E0u6SNsv+XLTOcr+H9g0cvW0QM8x
AcPs3hEtF10fuFDRXhmnad4HMyjKUJX5p1TLVIZQRan5SQIDAQABo0IwQDAPBgNV
HRMBAf8EBTADAQH/MB0GA1UdDgQWBBQIds3LB/8k9sXN7buQvOKEN0Z19zAOBgNV
HQ8BAf8EBAMCAQYwDQYJKoZIhvcNAQEFBQADggEBAKaorSLOAT2mo/9i0Eidi15y
sHhE49wcrwn9I0j6vSrEuVUEtRCjjSfeC4Jj0O7eDDd5QVsisrCaQVymcODU0HfL
I9MA4GxWL+FpDQ3Zqr8hgVDZBqWo/5U30Kr+4rP1mS1FhIrlQgnXdAIv94nYmem8
J9RHjboNRhx3zxSkHLmkMcScKHQDNP8zGSal6Q10tz6XxnboJ5ajZt3hrvJBW8qY
VoNzcOSGGtIxQbovvi0TWnZvTuhOgQ4/WwMioBK+ZlgRSssDxLQqKi2WF+A5VLxI
03YnnZotBqbJ7DnSq9ufmgsnAjUpsUCV5/nonFWIGUbWtzT1fs45mtk48VH3Tyw=
-----END CERTIFICATE-----

# Issuer: CN=Certinomis - Autorité Racine O=Certinomis OU=0002 433998903
# Subject: CN=Certinomis - Autorité Racine O=Certinomis OU=0002 433998903
# Label: "Certinomis - Autorité Racine"
# Serial: 1
# MD5 Fingerprint: 7f:30:78:8c:03:e3:ca:c9:0a:e2:c9:ea:1e:aa:55:1a
# SHA1 Fingerprint: 2e:14:da:ec:28:f0:fa:1e:8e:38:9a:4e:ab:eb:26:c0:0a:d3:83:c3
# SHA256 Fingerprint: fc:bf:e2:88:62:06:f7:2b:27:59:3c:8b:07:02:97:e1:2d:76:9e:d1:0e:d7:93:07:05:a8:09:8e:ff:c1:4d:17
-----BEGIN CERTIFICATE-----
MIIFnDCCA4SgAwIBAgIBATANBgkqhkiG9w0BAQUFADBjMQswCQYDVQQGEwJGUjET
MBEGA1UEChMKQ2VydGlub21pczEXMBUGA1UECxMOMDAwMiA0MzM5OTg5MDMxJjAk
BgNVBAMMHUNlcnRpbm9taXMgLSBBdXRvcml0w6kgUmFjaW5lMB4XDTA4MDkxNzA4
Mjg1OVoXDTI4MDkxNzA4Mjg1OVowYzELMAkGA1UEBhMCRlIxEzARBgNVBAoTCkNl
cnRpbm9taXMxFzAVBgNVBAsTDjAwMDIgNDMzOTk4OTAzMSYwJAYDVQQDDB1DZXJ0
aW5vbWlzIC0gQXV0b3JpdMOpIFJhY2luZTCCAiIwDQYJKoZIhvcNAQEBBQADggIP
ADCCAgoCggIBAJ2Fn4bT46/HsmtuM+Cet0I0VZ35gb5j2CN2DpdUzZlMGvE5x4jY
F1AMnmHawE5V3udauHpOd4cN5bjr+p5eex7Ezyh0x5P1FMYiKAT5kcOrJ3NqDi5N
8y4oH3DfVS9O7cdxbwlyLu3VMpfQ8Vh30WC8Tl7bmoT2R2FFK/ZQpn9qcSdIhDWe
rP5pqZ56XjUl+rSnSTV3lqc2W+HN3yNw2F1MpQiD8aYkOBOo7C+ooWfHpi2GR+6K
/OybDnT0K0kCe5B1jPyZOQE51kqJ5Z52qz6WKDgmi92NjMD2AR5vpTESOH2VwnHu
7XSu5DaiQ3XV8QCb4uTXzEIDS3h65X27uK4uIJPT5GHfceF2Z5c/tt9qc1pkIuVC
28+BA5PY9OMQ4HL2AHCs8MF6DwV/zzRpRbWT5BnbUhYjBYkOjUjkJW+zeL9i9Qf6
lSTClrLooyPCXQP8w9PlfMl1I9f09bze5N/NgL+RiH2nE7Q5uiy6vdFrzPOlKO1E
nn1So2+WLhl+HPNbxxaOu2B9d2ZHVIIAEWBsMsGoOBvrbpgT1u449fCfDu/+MYHB
0iSVL1N6aaLwD4ZFjliCK0wi1F6g530mJ0jfJUaNSih8hp75mxpZuWW/Bd22Ql09
5gBIgl4g9xGC3srYn+Y3RyYe63j3YcNBZFgCQfna4NH4+ej9Uji29YnfAgMBAAGj
WzBZMA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMB0GA1UdDgQWBBQN
jLZh2kS40RR9w759XkjwzspqsDAXBgNVHSAEEDAOMAwGCiqBegFWAgIAAQEwDQYJ
KoZIhvcNAQEFBQADggIBACQ+YAZ+He86PtvqrxyaLAEL9MW12Ukx9F1BjYkMTv9s
ov3/4gbIOZ/xWqndIlgVqIrTseYyCYIDbNc/CMf4uboAbbnW/FIyXaR/pDGUu7ZM
OH8oMDX/nyNTt7buFHAAQCvaR6s0fl6nVjBhK4tDrP22iCj1a7Y+YEq6QpA0Z43q
619FVDsXrIvkxmUP7tCMXWY5zjKn2BCXwH40nJ+U8/aGH88bc62UeYdocMMzpXDn
2NU4lG9jeeu/Cg4I58UvD0KgKxRA/yHgBcUn4YQRE7rWhh1BCxMjidPJC+iKunqj
o3M3NYB9Ergzd0A4wPpeMNLytqOx1qKVl4GbUu1pTP+A5FPbVFsDbVRfsbjvJL1v
nxHDx2TCDyhihWZeGnuyt++uNckZM6i4J9szVb9o4XVIRFb7zdNIu0eJOqxp9YDG
5ERQL1TEqkPFMTFYvZbF6nVsmnWxTfj3l/+WFvKXTej28xH5On2KOG4Ey+HTRRWq
pdEdnV1j6CTmNhTih60bWfVEm/vXd3wfAXBioSAaosUaKPQhA+4u2cGA6rnZgtZb
dsLLO7XSAPCjDuGtbkD326C00EauFddEwk01+dIL8hf2rGbVJLJP0RyZwG71fet0
BLj5TXcJ17TPBzAJ8bgAVtkXFhYKK4bfjwEZGuW7gmP/vgt2Fl43N+bYdJeimUV5
-----END CERTIFICATE-----

# Issuer: CN=Root CA Generalitat Valenciana O=Generalitat Valenciana OU=PKIGVA
# Subject: CN=Root CA Generalitat Valenciana O=Generalitat Valenciana OU=PKIGVA
# Label: "Root CA Generalitat Valenciana"
# Serial: 994436456
# MD5 Fingerprint: 2c:8c:17:5e:b1:54:ab:93:17:b5:36:5a:db:d1:c6:f2
# SHA1 Fingerprint: a0:73:e5:c5:bd:43:61:0d:86:4c:21:13:0a:85:58:57:cc:9c:ea:46
# SHA256 Fingerprint: 8c:4e:df:d0:43:48:f3:22:96:9e:7e:29:a4:cd:4d:ca:00:46:55:06:1c:16:e1:b0:76:42:2e:f3:42:ad:63:0e
-----BEGIN CERTIFICATE-----
MIIGizCCBXOgAwIBAgIEO0XlaDANBgkqhkiG9w0BAQUFADBoMQswCQYDVQQGEwJF
UzEfMB0GA1UEChMWR2VuZXJhbGl0YXQgVmFsZW5jaWFuYTEPMA0GA1UECxMGUEtJ
R1ZBMScwJQYDVQQDEx5Sb290IENBIEdlbmVyYWxpdGF0IFZhbGVuY2lhbmEwHhcN
MDEwNzA2MTYyMjQ3WhcNMjEwNzAxMTUyMjQ3WjBoMQswCQYDVQQGEwJFUzEfMB0G
A1UEChMWR2VuZXJhbGl0YXQgVmFsZW5jaWFuYTEPMA0GA1UECxMGUEtJR1ZBMScw
JQYDVQQDEx5Sb290IENBIEdlbmVyYWxpdGF0IFZhbGVuY2lhbmEwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQDGKqtXETcvIorKA3Qdyu0togu8M1JAJke+
WmmmO3I2F0zo37i7L3bhQEZ0ZQKQUgi0/6iMweDHiVYQOTPvaLRfX9ptI6GJXiKj
SgbwJ/BXufjpTjJ3Cj9BZPPrZe52/lSqfR0grvPXdMIKX/UIKFIIzFVd0g/bmoGl
u6GzwZTNVOAydTGRGmKy3nXiz0+J2ZGQD0EbtFpKd71ng+CT516nDOeB0/RSrFOy
A8dEJvt55cs0YFAQexvba9dHq198aMpunUEDEO5rmXteJajCq+TA81yc477OMUxk
Hl6AovWDfgzWyoxVjr7gvkkHD6MkQXpYHYTqWBLI4bft75PelAgxAgMBAAGjggM7
MIIDNzAyBggrBgEFBQcBAQQmMCQwIgYIKwYBBQUHMAGGFmh0dHA6Ly9vY3NwLnBr
aS5ndmEuZXMwEgYDVR0TAQH/BAgwBgEB/wIBAjCCAjQGA1UdIASCAiswggInMIIC
IwYKKwYBBAG/VQIBADCCAhMwggHoBggrBgEFBQcCAjCCAdoeggHWAEEAdQB0AG8A
cgBpAGQAYQBkACAAZABlACAAQwBlAHIAdABpAGYAaQBjAGEAYwBpAPMAbgAgAFIA
YQDtAHoAIABkAGUAIABsAGEAIABHAGUAbgBlAHIAYQBsAGkAdABhAHQAIABWAGEA
bABlAG4AYwBpAGEAbgBhAC4ADQAKAEwAYQAgAEQAZQBjAGwAYQByAGEAYwBpAPMA
bgAgAGQAZQAgAFAAcgDhAGMAdABpAGMAYQBzACAAZABlACAAQwBlAHIAdABpAGYA
aQBjAGEAYwBpAPMAbgAgAHEAdQBlACAAcgBpAGcAZQAgAGUAbAAgAGYAdQBuAGMA
aQBvAG4AYQBtAGkAZQBuAHQAbwAgAGQAZQAgAGwAYQAgAHAAcgBlAHMAZQBuAHQA
ZQAgAEEAdQB0AG8AcgBpAGQAYQBkACAAZABlACAAQwBlAHIAdABpAGYAaQBjAGEA
YwBpAPMAbgAgAHMAZQAgAGUAbgBjAHUAZQBuAHQAcgBhACAAZQBuACAAbABhACAA
ZABpAHIAZQBjAGMAaQDzAG4AIAB3AGUAYgAgAGgAdAB0AHAAOgAvAC8AdwB3AHcA
LgBwAGsAaQAuAGcAdgBhAC4AZQBzAC8AYwBwAHMwJQYIKwYBBQUHAgEWGWh0dHA6
Ly93d3cucGtpLmd2YS5lcy9jcHMwHQYDVR0OBBYEFHs100DSHHgZZu90ECjcPk+y
eAT8MIGVBgNVHSMEgY0wgYqAFHs100DSHHgZZu90ECjcPk+yeAT8oWykajBoMQsw
CQYDVQQGEwJFUzEfMB0GA1UEChMWR2VuZXJhbGl0YXQgVmFsZW5jaWFuYTEPMA0G
A1UECxMGUEtJR1ZBMScwJQYDVQQDEx5Sb290IENBIEdlbmVyYWxpdGF0IFZhbGVu
Y2lhbmGCBDtF5WgwDQYJKoZIhvcNAQEFBQADggEBACRhTvW1yEICKrNcda3Fbcrn
lD+laJWIwVTAEGmiEi8YPyVQqHxK6sYJ2fR1xkDar1CdPaUWu20xxsdzCkj+IHLt
b8zog2EWRpABlUt9jppSCS/2bxzkoXHPjCpaF3ODR00PNvsETUlR4hTJZGH71BTg
9J63NI8KJr2XXPR5OkowGcytT6CYirQxlyric21+eLj4iIlPsSKRZEv1UN4D2+XF
ducTZnV+ZfsBn5OHiJ35Rld8TWCvmHMTI6QgkYH60GFmuH3Rr9ZvHmw96RH9qfmC
IoaZM3Fa6hlXPZHNqcCjbgcTpsnt+GijnsNacgmHKNHEc8RzGF9QdRYxn7fofMM=
-----END CERTIFICATE-----

# Issuer: CN=A-Trust-nQual-03 O=A-Trust Ges. f. Sicherheitssysteme im elektr. Datenverkehr GmbH OU=A-Trust-nQual-03
# Subject: CN=A-Trust-nQual-03 O=A-Trust Ges. f. Sicherheitssysteme im elektr. Datenverkehr GmbH OU=A-Trust-nQual-03
# Label: "A-Trust-nQual-03"
# Serial: 93214
# MD5 Fingerprint: 49:63:ae:27:f4:d5:95:3d:d8:db:24:86:b8:9c:07:53
# SHA1 Fingerprint: d3:c0:63:f2:19:ed:07:3e:34:ad:5d:75:0b:32:76:29:ff:d5:9a:f2
# SHA256 Fingerprint: 79:3c:bf:45:59:b9:fd:e3:8a:b2:2d:f1:68:69:f6:98:81:ae:14:c4:b0:13:9a:c7:88:a7:8a:1a:fc:ca:02:fb
-----BEGIN CERTIFICATE-----
MIIDzzCCAregAwIBAgIDAWweMA0GCSqGSIb3DQEBBQUAMIGNMQswCQYDVQQGEwJB
VDFIMEYGA1UECgw/QS1UcnVzdCBHZXMuIGYuIFNpY2hlcmhlaXRzc3lzdGVtZSBp
bSBlbGVrdHIuIERhdGVudmVya2VociBHbWJIMRkwFwYDVQQLDBBBLVRydXN0LW5R
dWFsLTAzMRkwFwYDVQQDDBBBLVRydXN0LW5RdWFsLTAzMB4XDTA1MDgxNzIyMDAw
MFoXDTE1MDgxNzIyMDAwMFowgY0xCzAJBgNVBAYTAkFUMUgwRgYDVQQKDD9BLVRy
dXN0IEdlcy4gZi4gU2ljaGVyaGVpdHNzeXN0ZW1lIGltIGVsZWt0ci4gRGF0ZW52
ZXJrZWhyIEdtYkgxGTAXBgNVBAsMEEEtVHJ1c3QtblF1YWwtMDMxGTAXBgNVBAMM
EEEtVHJ1c3QtblF1YWwtMDMwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIB
AQCtPWFuA/OQO8BBC4SAzewqo51ru27CQoT3URThoKgtUaNR8t4j8DRE/5TrzAUj
lUC5B3ilJfYKvUWG6Nm9wASOhURh73+nyfrBJcyFLGM/BWBzSQXgYHiVEEvc+RFZ
znF/QJuKqiTfC0Li21a8StKlDJu3Qz7dg9MmEALP6iPESU7l0+m0iKsMrmKS1GWH
2WrX9IWf5DMiJaXlyDO6w8dB3F/GaswADm0yqLaHNgBid5seHzTLkDx4iHQF63n1
k3Flyp3HaxgtPVxO59X4PzF9j4fsCiIvI+n+u33J4PTs63zEsMMtYrWacdaxaujs
2e3Vcuy+VwHOBVWf3tFgiBCzAgMBAAGjNjA0MA8GA1UdEwEB/wQFMAMBAf8wEQYD
VR0OBAoECERqlWdVeRFPMA4GA1UdDwEB/wQEAwIBBjANBgkqhkiG9w0BAQUFAAOC
AQEAVdRU0VlIXLOThaq/Yy/kgM40ozRiPvbY7meIMQQDbwvUB/tOdQ/TLtPAF8fG
KOwGDREkDg6lXb+MshOWcdzUzg4NCmgybLlBMRmrsQd7TZjTXLDR8KdCoLXEjq/+
8T/0709GAHbrAvv5ndJAlseIOrifEXnzgGWovR/TeIGgUUw3tKZdJXDRZslo+S4R
FGjxVJgIrCaSD96JntT6s3kr0qN51OyLrIdTaEJMUVF0HhsnLuP1Hyl0Te2v9+GS
mYHovjrHF1D2t8b8m7CKa9aIA5GPBnc6hQLdmNVDeD/GMBWsm2vLV7eJUYs66MmE
DNuxUCAKGkq6ahq97BvIxYSazQ==
-----END CERTIFICATE-----

# Issuer: CN=TWCA Root Certification Authority O=TAIWAN-CA OU=Root CA
# Subject: CN=TWCA Root Certification Authority O=TAIWAN-CA OU=Root CA
# Label: "TWCA Root Certification Authority"
# Serial: 1
# MD5 Fingerprint: aa:08:8f:f6:f9:7b:b7:f2:b1:a7:1e:9b:ea:ea:bd:79
# SHA1 Fingerprint: cf:9e:87:6d:d3:eb:fc:42:26:97:a3:b5:a3:7a:a0:76:a9:06:23:48
# SHA256 Fingerprint: bf:d8:8f:e1:10:1c:41:ae:3e:80:1b:f8:be:56:35:0e:e9:ba:d1:a6:b9:bd:51:5e:dc:5c:6d:5b:87:11:ac:44
-----BEGIN CERTIFICATE-----
MIIDezCCAmOgAwIBAgIBATANBgkqhkiG9w0BAQUFADBfMQswCQYDVQQGEwJUVzES
MBAGA1UECgwJVEFJV0FOLUNBMRAwDgYDVQQLDAdSb290IENBMSowKAYDVQQDDCFU
V0NBIFJvb3QgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwHhcNMDgwODI4MDcyNDMz
WhcNMzAxMjMxMTU1OTU5WjBfMQswCQYDVQQGEwJUVzESMBAGA1UECgwJVEFJV0FO
LUNBMRAwDgYDVQQLDAdSb290IENBMSowKAYDVQQDDCFUV0NBIFJvb3QgQ2VydGlm
aWNhdGlvbiBBdXRob3JpdHkwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIB
AQCwfnK4pAOU5qfeCTiRShFAh6d8WWQUe7UREN3+v9XAu1bihSX0NXIP+FPQQeFE
AcK0HMMxQhZHhTMidrIKbw/lJVBPhYa+v5guEGcevhEFhgWQxFnQfHgQsIBct+HH
K3XLfJ+utdGdIzdjp9xCoi2SBBtQwXu4PhvJVgSLL1KbralW6cH/ralYhzC2gfeX
RfwZVzsrb+RH9JlF/h3x+JejiB03HFyP4HYlmlD4oFT/RJB2I9IyxsOrBr/8+7/z
rX2SYgJbKdM1o5OaQ2RgXbL6Mv87BK9NQGr5x+PvI/1ry+UPizgN7gr8/g+YnzAx
3WxSZfmLgb4i4RxYA7qRG4kHAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwIBBjAPBgNV
HRMBAf8EBTADAQH/MB0GA1UdDgQWBBRqOFsmjd6LWvJPelSDGRjjCDWmujANBgkq
hkiG9w0BAQUFAAOCAQEAPNV3PdrfibqHDAhUaiBQkr6wQT25JmSDCi/oQMCXKCeC
MErJk/9q56YAf4lCmtYR5VPOL8zy2gXE/uJQxDqGfczafhAJO5I1KlOy/usrBdls
XebQ79NqZp4VKIV66IIArB6nCWlWQtNoURi+VJq/REG6Sb4gumlc7rh3zc5sH62D
lhh9DrUUOYTxKOkto557HnpyWoOzeW/vtPzQCqVYT0bf+215WfKEIlKuD8z7fDvn
aspHYcN6+NOSBB+4IIThNlQWx0DeO4pz3N/GCUzf7Nr/1FNCocnyYh0igzyXxfkZ
YiesZSLX0zzG5Y6yU8xJzrww/nsOM5D77dIUkR8Hrw==
-----END CERTIFICATE-----

# Issuer: O=SECOM Trust Systems CO.,LTD. OU=Security Communication RootCA2
# Subject: O=SECOM Trust Systems CO.,LTD. OU=Security Communication RootCA2
# Label: "Security Communication RootCA2"
# Serial: 0
# MD5 Fingerprint: 6c:39:7d:a4:0e:55:59:b2:3f:d6:41:b1:12:50:de:43
# SHA1 Fingerprint: 5f:3b:8c:f2:f8:10:b3:7d:78:b4:ce:ec:19:19:c3:73:34:b9:c7:74
# SHA256 Fingerprint: 51:3b:2c:ec:b8:10:d4:cd:e5:dd:85:39:1a:df:c6:c2:dd:60:d8:7b:b7:36:d2:b5:21:48:4a:a4:7a:0e:be:f6
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIBADANBgkqhkiG9w0BAQsFADBdMQswCQYDVQQGEwJKUDEl
MCMGA1UEChMcU0VDT00gVHJ1c3QgU3lzdGVtcyBDTy4sTFRELjEnMCUGA1UECxMe
U2VjdXJpdHkgQ29tbXVuaWNhdGlvbiBSb290Q0EyMB4XDTA5MDUyOTA1MDAzOVoX
DTI5MDUyOTA1MDAzOVowXTELMAkGA1UEBhMCSlAxJTAjBgNVBAoTHFNFQ09NIFRy
dXN0IFN5c3RlbXMgQ08uLExURC4xJzAlBgNVBAsTHlNlY3VyaXR5IENvbW11bmlj
YXRpb24gUm9vdENBMjCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBANAV
OVKxUrO6xVmCxF1SrjpDZYBLx/KWvNs2l9amZIyoXvDjChz335c9S672XewhtUGr
zbl+dp+++T42NKA7wfYxEUV0kz1XgMX5iZnK5atq1LXaQZAQwdbWQonCv/Q4EpVM
VAX3NuRFg3sUZdbcDE3R3n4MqzvEFb46VqZab3ZpUql6ucjrappdUtAtCms1FgkQ
hNBqyjoGADdH5H5XTz+L62e4iKrFvlNVspHEfbmwhRkGeC7bYRr6hfVKkaHnFtWO
ojnflLhwHyg/i/xAXmODPIMqGplrz95Zajv8bxbXH/1KEOtOghY6rCcMU/Gt1SSw
awNQwS08Ft1ENCcadfsCAwEAAaNCMEAwHQYDVR0OBBYEFAqFqXdlBZh8QIH4D5cs
OPEK7DzPMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/MA0GCSqGSIb3
DQEBCwUAA4IBAQBMOqNErLlFsceTfsgLCkLfZOoc7llsCLqJX2rKSpWeeo8HxdpF
coJxDjrSzG+ntKEju/Ykn8sX/oymzsLS28yN/HH8AynBbF0zX2S2ZTuJbxh2ePXc
okgfGT+Ok+vx+hfuzU7jBBJV1uXk3fs+BXziHV7Gp7yXT2g69ekuCkO2r1dcYmh8
t/2jioSgrGK+KwmHNPBqAbubKVY8/gA3zyNs8U6qtnRGEmyR7jTV7JqR50S+kDFy
1UkC9gLl9B/rfNmWVan/7Ir5mUf/NVoCqgTLiluHcSmRvaS0eg29mvVXIwAHIRc/
SjnRBUkLp7Y3gaVdjKozXoEofKd9J+sAro03
-----END CERTIFICATE-----

# Issuer: CN=Hellenic Academic and Research Institutions RootCA 2011 O=Hellenic Academic and Research Institutions Cert. Authority
# Subject: CN=Hellenic Academic and Research Institutions RootCA 2011 O=Hellenic Academic and Research Institutions Cert. Authority
# Label: "Hellenic Academic and Research Institutions RootCA 2011"
# Serial: 0
# MD5 Fingerprint: 73:9f:4c:4b:73:5b:79:e9:fa:ba:1c:ef:6e:cb:d5:c9
# SHA1 Fingerprint: fe:45:65:9b:79:03:5b:98:a1:61:b5:51:2e:ac:da:58:09:48:22:4d
# SHA256 Fingerprint: bc:10:4f:15:a4:8b:e7:09:dc:a5:42:a7:e1:d4:b9:df:6f:05:45:27:e8:02:ea:a9:2d:59:54:44:25:8a:fe:71
-----BEGIN CERTIFICATE-----
MIIEMTCCAxmgAwIBAgIBADANBgkqhkiG9w0BAQUFADCBlTELMAkGA1UEBhMCR1Ix
RDBCBgNVBAoTO0hlbGxlbmljIEFjYWRlbWljIGFuZCBSZXNlYXJjaCBJbnN0aXR1
dGlvbnMgQ2VydC4gQXV0aG9yaXR5MUAwPgYDVQQDEzdIZWxsZW5pYyBBY2FkZW1p
YyBhbmQgUmVzZWFyY2ggSW5zdGl0dXRpb25zIFJvb3RDQSAyMDExMB4XDTExMTIw
NjEzNDk1MloXDTMxMTIwMTEzNDk1MlowgZUxCzAJBgNVBAYTAkdSMUQwQgYDVQQK
EztIZWxsZW5pYyBBY2FkZW1pYyBhbmQgUmVzZWFyY2ggSW5zdGl0dXRpb25zIENl
cnQuIEF1dGhvcml0eTFAMD4GA1UEAxM3SGVsbGVuaWMgQWNhZGVtaWMgYW5kIFJl
c2VhcmNoIEluc3RpdHV0aW9ucyBSb290Q0EgMjAxMTCCASIwDQYJKoZIhvcNAQEB
BQADggEPADCCAQoCggEBAKlTAOMupvaO+mDYLZU++CwqVE7NuYRhlFhPjz2L5EPz
dYmNUeTDN9KKiE15HrcS3UN4SoqS5tdI1Q+kOilENbgH9mgdVc04UfCMJDGFr4PJ
fel3r+0ae50X+bOdOFAPplp5kYCvN66m0zH7tSYJnTxa71HFK9+WXesyHgLacEns
bgzImjeN9/E2YEsmLIKe0HjzDQ9jpFEw4fkrJxIH2Oq9GGKYsFk3fb7u8yBRQlqD
75O6aRXxYp2fmTmCobd0LovUxQt7L/DICto9eQqakxylKHJzkUOap9FNhYS5qXSP
FEDH3N6sQWRstBmbAmNtJGSPRLIl6s5ddAxjMlyNh+UCAwEAAaOBiTCBhjAPBgNV
HRMBAf8EBTADAQH/MAsGA1UdDwQEAwIBBjAdBgNVHQ4EFgQUppFC/RNhSiOeCKQp
5dgTBCPuQSUwRwYDVR0eBEAwPqA8MAWCAy5ncjAFggMuZXUwBoIELmVkdTAGggQu
b3JnMAWBAy5ncjAFgQMuZXUwBoEELmVkdTAGgQQub3JnMA0GCSqGSIb3DQEBBQUA
A4IBAQAf73lB4XtuP7KMhjdCSk4cNx6NZrokgclPEg8hwAOXhiVtXdMiKahsog2p
6z0GW5k6x8zDmjR/qw7IThzh+uTczQ2+vyT+bOdrwg3IBp5OjWEopmr95fZi6hg8
TqBTnbI6nOulnJEWtk2C4AwFSKls9cz4y51JtPACpf1wA+2KIaWuE4ZJwzNzvoc7
dIsXRSZMFpGD/md9zU1jZ/rzAxKWeAaNsWftjj++n08C9bMJL/NMh98qy5V8Acys
Nnq/onN694/BtZqhFLKPM58N7yLcZnuEvUUXBj08yrl3NI/K6s8/MT7jiOOASSXI
l7WdmplNsDz4SgCbZN2fOUvRJ9e4
-----END CERTIFICATE-----

# Issuer: CN=Actalis Authentication Root CA O=Actalis S.p.A./03358520967
# Subject: CN=Actalis Authentication Root CA O=Actalis S.p.A./03358520967
# Label: "Actalis Authentication Root CA"
# Serial: 6271844772424770508
# MD5 Fingerprint: 69:c1:0d:4f:07:a3:1b:c3:fe:56:3d:04:bc:11:f6:a6
# SHA1 Fingerprint: f3:73:b3:87:06:5a:28:84:8a:f2:f3:4a:ce:19:2b:dd:c7:8e:9c:ac
# SHA256 Fingerprint: 55:92:60:84:ec:96:3a:64:b9:6e:2a:be:01:ce:0b:a8:6a:64:fb:fe:bc:c7:aa:b5:af:c1:55:b3:7f:d7:60:66
-----BEGIN CERTIFICATE-----
MIIFuzCCA6OgAwIBAgIIVwoRl0LE48wwDQYJKoZIhvcNAQELBQAwazELMAkGA1UE
BhMCSVQxDjAMBgNVBAcMBU1pbGFuMSMwIQYDVQQKDBpBY3RhbGlzIFMucC5BLi8w
MzM1ODUyMDk2NzEnMCUGA1UEAwweQWN0YWxpcyBBdXRoZW50aWNhdGlvbiBSb290
IENBMB4XDTExMDkyMjExMjIwMloXDTMwMDkyMjExMjIwMlowazELMAkGA1UEBhMC
SVQxDjAMBgNVBAcMBU1pbGFuMSMwIQYDVQQKDBpBY3RhbGlzIFMucC5BLi8wMzM1
ODUyMDk2NzEnMCUGA1UEAwweQWN0YWxpcyBBdXRoZW50aWNhdGlvbiBSb290IENB
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAp8bEpSmkLO/lGMWwUKNv
UTufClrJwkg4CsIcoBh/kbWHuUA/3R1oHwiD1S0eiKD4j1aPbZkCkpAW1V8IbInX
4ay8IMKx4INRimlNAJZaby/ARH6jDuSRzVju3PvHHkVH3Se5CAGfpiEd9UEtL0z9
KK3giq0itFZljoZUj5NDKd45RnijMCO6zfB9E1fAXdKDa0hMxKufgFpbOr3JpyI/
gCczWw63igxdBzcIy2zSekciRDXFzMwujt0q7bd9Zg1fYVEiVRvjRuPjPdA1Yprb
rxTIW6HMiRvhMCb8oJsfgadHHwTrozmSBp+Z07/T6k9QnBn+locePGX2oxgkg4YQ
51Q+qDp2JE+BIcXjDwL4k5RHILv+1A7TaLndxHqEguNTVHnd25zS8gebLra8Pu2F
be8lEfKXGkJh90qX6IuxEAf6ZYGyojnP9zz/GPvG8VqLWeICrHuS0E4UT1lF9gxe
KF+w6D9Fz8+vm2/7hNN3WpVvrJSEnu68wEqPSpP4RCHiMUVhUE4Q2OM1fEwZtN4F
v6MGn8i1zeQf1xcGDXqVdFUNaBr8EBtiZJ1t4JWgw5QHVw0U5r0F+7if5t+L4sbn
fpb2U8WANFAoWPASUHEXMLrmeGO89LKtmyuy/uE5jF66CyCU3nuDuP/jVo23Eek7
jPKxwV2dpAtMK9myGPW1n0sCAwEAAaNjMGEwHQYDVR0OBBYEFFLYiDrIn3hm7Ynz
ezhwlMkCAjbQMA8GA1UdEwEB/wQFMAMBAf8wHwYDVR0jBBgwFoAUUtiIOsifeGbt
ifN7OHCUyQICNtAwDgYDVR0PAQH/BAQDAgEGMA0GCSqGSIb3DQEBCwUAA4ICAQAL
e3KHwGCmSUyIWOYdiPcUZEim2FgKDk8TNd81HdTtBjHIgT5q1d07GjLukD0R0i70
jsNjLiNmsGe+b7bAEzlgqqI0JZN1Ut6nna0Oh4lScWoWPBkdg/iaKWW+9D+a2fDz
WochcYBNy+A4mz+7+uAwTc+G02UQGRjRlwKxK3JCaKygvU5a2hi/a5iB0P2avl4V
SM0RFbnAKVy06Ij3Pjaut2L9HmLecHgQHEhb2rykOLpn7VU+Xlff1ANATIGk0k9j
pwlCCRT8AKnCgHNPLsBA2RF7SOp6AsDT6ygBJlh0wcBzIm2Tlf05fbsq4/aC4yyX
X04fkZT6/iyj2HYauE2yOE+b+h1IYHkm4vP9qdCa6HCPSXrW5b0KDtst842/6+Ok
fcvHlXHo2qN8xcL4dJIEG4aspCJTQLas/kx2z/uUMsA1n3Y/buWQbqCmJqK4LL7R
K4X9p2jIugErsWx0Hbhzlefut8cl8ABMALJ+tguLHPPAUJ4lueAI3jZm/zel0btU
ZCzJJ7VLkn5l/9Mt4blOvH+kQSGQQXemOR/qnuOf0GZvBeyqdn6/axag67XH/JJU
LysRJyU3eExRarDzzFhdFPFqSBX/wge2sY0PjlxQRrM9vwGYT7JZVEc+NHt4bVaT
LnPqZih4zR0Uv6CPLy64Lo7yFIrM6bV8+2ydDKXhlg==
-----END CERTIFICATE-----

# Issuer: O=Trustis Limited OU=Trustis FPS Root CA
# Subject: O=Trustis Limited OU=Trustis FPS Root CA
# Label: "Trustis FPS Root CA"
# Serial: 36053640375399034304724988975563710553
# MD5 Fingerprint: 30:c9:e7:1e:6b:e6:14:eb:65:b2:16:69:20:31:67:4d
# SHA1 Fingerprint: 3b:c0:38:0b:33:c3:f6:a6:0c:86:15:22:93:d9:df:f5:4b:81:c0:04
# SHA256 Fingerprint: c1:b4:82:99:ab:a5:20:8f:e9:63:0a:ce:55:ca:68:a0:3e:da:5a:51:9c:88:02:a0:d3:a6:73:be:8f:8e:55:7d
-----BEGIN CERTIFICATE-----
MIIDZzCCAk+gAwIBAgIQGx+ttiD5JNM2a/fH8YygWTANBgkqhkiG9w0BAQUFADBF
MQswCQYDVQQGEwJHQjEYMBYGA1UEChMPVHJ1c3RpcyBMaW1pdGVkMRwwGgYDVQQL
ExNUcnVzdGlzIEZQUyBSb290IENBMB4XDTAzMTIyMzEyMTQwNloXDTI0MDEyMTEx
MzY1NFowRTELMAkGA1UEBhMCR0IxGDAWBgNVBAoTD1RydXN0aXMgTGltaXRlZDEc
MBoGA1UECxMTVHJ1c3RpcyBGUFMgUm9vdCBDQTCCASIwDQYJKoZIhvcNAQEBBQAD
ggEPADCCAQoCggEBAMVQe547NdDfxIzNjpvto8A2mfRC6qc+gIMPpqdZh8mQRUN+
AOqGeSoDvT03mYlmt+WKVoaTnGhLaASMk5MCPjDSNzoiYYkchU59j9WvezX2fihH
iTHcDnlkH5nSW7r+f2C/revnPDgpai/lkQtV/+xvWNUtyd5MZnGPDNcE2gfmHhjj
vSkCqPoc4Vu5g6hBSLwacY3nYuUtsuvffM/bq1rKMfFMIvMFE/eC+XN5DL7XSxzA
0RU8k0Fk0ea+IxciAIleH2ulrG6nS4zto3Lmr2NNL4XSFDWaLk6M6jKYKIahkQlB
OrTh4/L68MkKokHdqeMDx4gVOxzUGpTXn2RZEm0CAwEAAaNTMFEwDwYDVR0TAQH/
BAUwAwEB/zAfBgNVHSMEGDAWgBS6+nEleYtXQSUhhgtx67JkDoshZzAdBgNVHQ4E
FgQUuvpxJXmLV0ElIYYLceuyZA6LIWcwDQYJKoZIhvcNAQEFBQADggEBAH5Y//01
GX2cGE+esCu8jowU/yyg2kdbw++BLa8F6nRIW/M+TgfHbcWzk88iNVy2P3UnXwmW
zaD+vkAMXBJV+JOCyinpXj9WV4s4NvdFGkwozZ5BuO1WTISkQMi4sKUraXAEasP4
1BIy+Q7DsdwyhEQsb8tGD+pmQQ9P8Vilpg0ND2HepZ5dfWWhPBfnqFVO76DH7cZE
f1T1o+CP8HxVIo8ptoGj4W1OLBuAZ+ytIJ8MYmHVl/9D7S3B2l0pKoU/rGXuhg8F
jZBf3+6f9L/uHfuY5H+QK4R4EA5sSVPvFVtlRkpdr7r7OnIdzfYliB6XzCGcKQEN
ZetX2fNXlrtIzYE=
-----END CERTIFICATE-----

# Issuer: CN=StartCom Certification Authority O=StartCom Ltd. OU=Secure Digital Certificate Signing
# Subject: CN=StartCom Certification Authority O=StartCom Ltd. OU=Secure Digital Certificate Signing
# Label: "StartCom Certification Authority"
# Serial: 45
# MD5 Fingerprint: c9:3b:0d:84:41:fc:a4:76:79:23:08:57:de:10:19:16
# SHA1 Fingerprint: a3:f1:33:3f:e2:42:bf:cf:c5:d1:4e:8f:39:42:98:40:68:10:d1:a0
# SHA256 Fingerprint: e1:78:90:ee:09:a3:fb:f4:f4:8b:9c:41:4a:17:d6:37:b7:a5:06:47:e9:bc:75:23:22:72:7f:cc:17:42:a9:11
-----BEGIN CERTIFICATE-----
MIIHhzCCBW+gAwIBAgIBLTANBgkqhkiG9w0BAQsFADB9MQswCQYDVQQGEwJJTDEW
MBQGA1UEChMNU3RhcnRDb20gTHRkLjErMCkGA1UECxMiU2VjdXJlIERpZ2l0YWwg
Q2VydGlmaWNhdGUgU2lnbmluZzEpMCcGA1UEAxMgU3RhcnRDb20gQ2VydGlmaWNh
dGlvbiBBdXRob3JpdHkwHhcNMDYwOTE3MTk0NjM3WhcNMzYwOTE3MTk0NjM2WjB9
MQswCQYDVQQGEwJJTDEWMBQGA1UEChMNU3RhcnRDb20gTHRkLjErMCkGA1UECxMi
U2VjdXJlIERpZ2l0YWwgQ2VydGlmaWNhdGUgU2lnbmluZzEpMCcGA1UEAxMgU3Rh
cnRDb20gQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkwggIiMA0GCSqGSIb3DQEBAQUA
A4ICDwAwggIKAoICAQDBiNsJvGxGfHiflXu1M5DycmLWwTYgIiRezul38kMKogZk
pMyONvg45iPwbm2xPN1yo4UcodM9tDMr0y+v/uqwQVlntsQGfQqedIXWeUyAN3rf
OQVSWff0G0ZDpNKFhdLDcfN1YjS6LIp/Ho/u7TTQEceWzVI9ujPW3U3eCztKS5/C
Ji/6tRYccjV3yjxd5srhJosaNnZcAdt0FCX+7bWgiA/deMotHweXMAEtcnn6RtYT
Kqi5pquDSR3l8u/d5AGOGAqPY1MWhWKpDhk6zLVmpsJrdAfkK+F2PrRt2PZE4XNi
HzvEvqBTViVsUQn3qqvKv3b9bZvzndu/PWa8DFaqr5hIlTpL36dYUNk4dalb6kMM
Av+Z6+hsTXBbKWWc3apdzK8BMewM69KN6Oqce+Zu9ydmDBpI125C4z/eIT574Q1w
+2OqqGwaVLRcJXrJosmLFqa7LH4XXgVNWG4SHQHuEhANxjJ/GP/89PrNbpHoNkm+
Gkhpi8KWTRoSsmkXwQqQ1vp5Iki/untp+HDH+no32NgN0nZPV/+Qt+OR0t3vwmC3
Zzrd/qqc8NSLf3Iizsafl7b4r4qgEKjZ+xjGtrVcUjyJthkqcwEKDwOzEmDyei+B
26Nu/yYwl/WL3YlXtq09s68rxbd2AvCl1iuahhQqcvbjM4xdCUsT37uMdBNSSwID
AQABo4ICEDCCAgwwDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAQYwHQYD
VR0OBBYEFE4L7xqkQFulF2mHMMo0aEPQQa7yMB8GA1UdIwQYMBaAFE4L7xqkQFul
F2mHMMo0aEPQQa7yMIIBWgYDVR0gBIIBUTCCAU0wggFJBgsrBgEEAYG1NwEBATCC
ATgwLgYIKwYBBQUHAgEWImh0dHA6Ly93d3cuc3RhcnRzc2wuY29tL3BvbGljeS5w
ZGYwNAYIKwYBBQUHAgEWKGh0dHA6Ly93d3cuc3RhcnRzc2wuY29tL2ludGVybWVk
aWF0ZS5wZGYwgc8GCCsGAQUFBwICMIHCMCcWIFN0YXJ0IENvbW1lcmNpYWwgKFN0
YXJ0Q29tKSBMdGQuMAMCAQEagZZMaW1pdGVkIExpYWJpbGl0eSwgcmVhZCB0aGUg
c2VjdGlvbiAqTGVnYWwgTGltaXRhdGlvbnMqIG9mIHRoZSBTdGFydENvbSBDZXJ0
aWZpY2F0aW9uIEF1dGhvcml0eSBQb2xpY3kgYXZhaWxhYmxlIGF0IGh0dHA6Ly93
d3cuc3RhcnRzc2wuY29tL3BvbGljeS5wZGYwEQYJYIZIAYb4QgEBBAQDAgAHMDgG
CWCGSAGG+EIBDQQrFilTdGFydENvbSBGcmVlIFNTTCBDZXJ0aWZpY2F0aW9uIEF1
dGhvcml0eTANBgkqhkiG9w0BAQsFAAOCAgEAjo/n3JR5fPGFf59Jb2vKXfuM/gTF
wWLRfUKKvFO3lANmMD+x5wqnUCBVJX92ehQN6wQOQOY+2IirByeDqXWmN3PH/UvS
Ta0XQMhGvjt/UfzDtgUx3M2FIk5xt/JxXrAaxrqTi3iSSoX4eA+D/i+tLPfkpLst
0OcNOrg+zvZ49q5HJMqjNTbOx8aHmNrs++myziebiMMEofYLWWivydsQD032ZGNc
pRJvkrKTlMeIFw6Ttn5ii5B/q06f/ON1FE8qMt9bDeD1e5MNq6HPh+GlBEXoPBKl
CcWw0bdT82AUuoVpaiF8H3VhFyAXe2w7QSlc4axa0c2Mm+tgHRns9+Ww2vl5GKVF
P0lDV9LdJNUso/2RjSe15esUBppMeyG7Oq0wBhjA2MFrLH9ZXF2RsXAiV+uKa0hK
1Q8p7MZAwC+ITGgBF3f0JBlPvfrhsiAhS90a2Cl9qrjeVOwhVYBsHvUwyKMQ5bLm
KhQxw4UtjJixhlpPiVktucf3HMiKf8CdBUrmQk9io20ppB+Fq9vlgcitKj1MXVuE
JnHEhV5xJMqlG2zYYdMa4FTbzrqpMrUi9nNBCV24F10OD5mQ1kfabwo6YigUZ4LZ
8dCAWZvLMdibD4x3TrVoivJs9iQOLWxwxXPR3hTQcY+203sC9uO41Alua551hDnm
fyWl8kgAwKQB2j8=
-----END CERTIFICATE-----

# Issuer: CN=StartCom Certification Authority G2 O=StartCom Ltd.
# Subject: CN=StartCom Certification Authority G2 O=StartCom Ltd.
# Label: "StartCom Certification Authority G2"
# Serial: 59
# MD5 Fingerprint: 78:4b:fb:9e:64:82:0a:d3:b8:4c:62:f3:64:f2:90:64
# SHA1 Fingerprint: 31:f1:fd:68:22:63:20:ee:c6:3b:3f:9d:ea:4a:3e:53:7c:7c:39:17
# SHA256 Fingerprint: c7:ba:65:67:de:93:a7:98:ae:1f:aa:79:1e:71:2d:37:8f:ae:1f:93:c4:39:7f:ea:44:1b:b7:cb:e6:fd:59:95
-----BEGIN CERTIFICATE-----
MIIFYzCCA0ugAwIBAgIBOzANBgkqhkiG9w0BAQsFADBTMQswCQYDVQQGEwJJTDEW
MBQGA1UEChMNU3RhcnRDb20gTHRkLjEsMCoGA1UEAxMjU3RhcnRDb20gQ2VydGlm
aWNhdGlvbiBBdXRob3JpdHkgRzIwHhcNMTAwMTAxMDEwMDAxWhcNMzkxMjMxMjM1
OTAxWjBTMQswCQYDVQQGEwJJTDEWMBQGA1UEChMNU3RhcnRDb20gTHRkLjEsMCoG
A1UEAxMjU3RhcnRDb20gQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkgRzIwggIiMA0G
CSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQC2iTZbB7cgNr2Cu+EWIAOVeq8Oo1XJ
JZlKxdBWQYeQTSFgpBSHO839sj60ZwNq7eEPS8CRhXBF4EKe3ikj1AENoBB5uNsD
vfOpL9HG4A/LnooUCri99lZi8cVytjIl2bLzvWXFDSxu1ZJvGIsAQRSCb0AgJnoo
D/Uefyf3lLE3PbfHkffiAez9lInhzG7TNtYKGXmu1zSCZf98Qru23QumNK9LYP5/
Q0kGi4xDuFby2X8hQxfqp0iVAXV16iulQ5XqFYSdCI0mblWbq9zSOdIxHWDirMxW
RST1HFSr7obdljKF+ExP6JV2tgXdNiNnvP8V4so75qbsO+wmETRIjfaAKxojAuuK
HDp2KntWFhxyKrOq42ClAJ8Em+JvHhRYW6Vsi1g8w7pOOlz34ZYrPu8HvKTlXcxN
nw3h3Kq74W4a7I/htkxNeXJdFzULHdfBR9qWJODQcqhaX2YtENwvKhOuJv4KHBnM
0D4LnMgJLvlblnpHnOl68wVQdJVznjAJ85eCXuaPOQgeWeU1FEIT/wCc976qUM/i
UUjXuG+v+E5+M5iSFGI6dWPPe/regjupuznixL0sAA7IF6wT700ljtizkC+p2il9
Ha90OrInwMEePnWjFqmveiJdnxMaz6eg6+OGCtP95paV1yPIN93EfKo2rJgaErHg
TuixO/XWb/Ew1wIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQE
AwIBBjAdBgNVHQ4EFgQUS8W0QGutHLOlHGVuRjaJhwUMDrYwDQYJKoZIhvcNAQEL
BQADggIBAHNXPyzVlTJ+N9uWkusZXn5T50HsEbZH77Xe7XRcxfGOSeD8bpkTzZ+K
2s06Ctg6Wgk/XzTQLwPSZh0avZyQN8gMjgdalEVGKua+etqhqaRpEpKwfTbURIfX
UfEpY9Z1zRbkJ4kd+MIySP3bmdCPX1R0zKxnNBFi2QwKN4fRoxdIjtIXHfbX/dtl
6/2o1PXWT6RbdejF0mCy2wl+JYt7ulKSnj7oxXehPOBKc2thz4bcQ///If4jXSRK
9dNtD2IEBVeC2m6kMyV5Sy5UGYvMLD0w6dEG/+gyRr61M3Z3qAFdlsHB1b6uJcDJ
HgoJIIihDsnzb02CVAAgp9KP5DlUFy6NHrgbuxu9mk47EDTcnIhT76IxW1hPkWLI
wpqazRVdOKnWvvgTtZ8SafJQYqz7Fzf07rh1Z2AQ+4NQ+US1dZxAF7L+/XldblhY
XzD8AK6vM8EOTmy6p6ahfzLbOOCxchcKK5HsamMm7YnUeMx0HgX4a/6ManY5Ka5l
IxKVCCIcl85bBu4M4ru8H0ST9tg4RQUh7eStqxK2A6RCLi3ECToDZ2mEmuFZkIoo
hdVddLHRDiBYmxOlsGOm7XtH/UVVMKTumtTm4ofvmMkyghEpIrwACjFeLQ/Ajulr
so8uBtjRkcfGEvRM/TAXw8HaOFvjqermobp573PYtlNXLfbQ4ddI
-----END CERTIFICATE-----

# Issuer: CN=Buypass Class 2 Root CA O=Buypass AS-983163327
# Subject: CN=Buypass Class 2 Root CA O=Buypass AS-983163327
# Label: "Buypass Class 2 Root CA"
# Serial: 2
# MD5 Fingerprint: 46:a7:d2:fe:45:fb:64:5a:a8:59:90:9b:78:44:9b:29
# SHA1 Fingerprint: 49:0a:75:74:de:87:0a:47:fe:58:ee:f6:c7:6b:eb:c6:0b:12:40:99
# SHA256 Fingerprint: 9a:11:40:25:19:7c:5b:b9:5d:94:e6:3d:55:cd:43:79:08:47:b6:46:b2:3c:df:11:ad:a4:a0:0e:ff:15:fb:48
-----BEGIN CERTIFICATE-----
MIIFWTCCA0GgAwIBAgIBAjANBgkqhkiG9w0BAQsFADBOMQswCQYDVQQGEwJOTzEd
MBsGA1UECgwUQnV5cGFzcyBBUy05ODMxNjMzMjcxIDAeBgNVBAMMF0J1eXBhc3Mg
Q2xhc3MgMiBSb290IENBMB4XDTEwMTAyNjA4MzgwM1oXDTQwMTAyNjA4MzgwM1ow
TjELMAkGA1UEBhMCTk8xHTAbBgNVBAoMFEJ1eXBhc3MgQVMtOTgzMTYzMzI3MSAw
HgYDVQQDDBdCdXlwYXNzIENsYXNzIDIgUm9vdCBDQTCCAiIwDQYJKoZIhvcNAQEB
BQADggIPADCCAgoCggIBANfHXvfBB9R3+0Mh9PT1aeTuMgHbo4Yf5FkNuud1g1Lr
6hxhFUi7HQfKjK6w3Jad6sNgkoaCKHOcVgb/S2TwDCo3SbXlzwx87vFKu3MwZfPV
L4O2fuPn9Z6rYPnT8Z2SdIrkHJasW4DptfQxh6NR/Md+oW+OU3fUl8FVM5I+GC91
1K2GScuVr1QGbNgGE41b/+EmGVnAJLqBcXmQRFBoJJRfuLMR8SlBYaNByyM21cHx
MlAQTn/0hpPshNOOvEu/XAFOBz3cFIqUCqTqc/sLUegTBxj6DvEr0VQVfTzh97QZ
QmdiXnfgolXsttlpF9U6r0TtSsWe5HonfOV116rLJeffawrbD02TTqigzXsu8lkB
arcNuAeBfos4GzjmCleZPe4h6KP1DBbdi+w0jpwqHAAVF41og9JwnxgIzRFo1clr
Us3ERo/ctfPYV3Me6ZQ5BL/T3jjetFPsaRyifsSP5BtwrfKi+fv3FmRmaZ9JUaLi
FRhnBkp/1Wy1TbMz4GHrXb7pmA8y1x1LPC5aAVKRCfLf6o3YBkBjqhHk/sM3nhRS
P/TizPJhk9H9Z2vXUq6/aKtAQ6BXNVN48FP4YUIHZMbXb5tMOA1jrGKvNouicwoN
9SG9dKpN6nIDSdvHXx1iY8f93ZHsM+71bbRuMGjeyNYmsHVee7QHIJihdjK4TWxP
AgMBAAGjQjBAMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFMmAd+BikoL1Rpzz
uvdMw964o605MA4GA1UdDwEB/wQEAwIBBjANBgkqhkiG9w0BAQsFAAOCAgEAU18h
9bqwOlI5LJKwbADJ784g7wbylp7ppHR/ehb8t/W2+xUbP6umwHJdELFx7rxP462s
A20ucS6vxOOto70MEae0/0qyexAQH6dXQbLArvQsWdZHEIjzIVEpMMpghq9Gqx3t
OluwlN5E40EIosHsHdb9T7bWR9AUC8rmyrV7d35BH16Dx7aMOZawP5aBQW9gkOLo
+fsicdl9sz1Gv7SEr5AcD48Saq/v7h56rgJKihcrdv6sVIkkLE8/trKnToyokZf7
KcZ7XC25y2a2t6hbElGFtQl+Ynhw/qlqYLYdDnkM/crqJIByw5c/8nerQyIKx+u2
DISCLIBrQYoIwOula9+ZEsuK1V6ADJHgJgg2SMX6OBE1/yWDLfJ6v9r9jv6ly0Us
H8SIU653DtmadsWOLB2jutXsMq7Aqqz30XpN69QH4kj3Io6wpJ9qzo6ysmD0oyLQ
I+uUWnpp3Q+/QFesa1lQ2aOZ4W7+jQF5JyMV3pKdewlNWudLSDBaGOYKbeaP4NK7
5t98biGCwWg5TbSYWGZizEqQXsP6JwSxeRV0mcy+rSDeJmAc61ZRpqPq5KM/p/9h
3PFaTWwyI0PurKju7koSCTxdccK+efrCh2gdC/1cacwG0Jp9VJkqyTkaGa9LKkPz
Y11aWOIv4x3kqdbQCtCev9eBCfHJxyYNrJgWVqA=
-----END CERTIFICATE-----

# Issuer: CN=Buypass Class 3 Root CA O=Buypass AS-983163327
# Subject: CN=Buypass Class 3 Root CA O=Buypass AS-983163327
# Label: "Buypass Class 3 Root CA"
# Serial: 2
# MD5 Fingerprint: 3d:3b:18:9e:2c:64:5a:e8:d5:88:ce:0e:f9:37:c2:ec
# SHA1 Fingerprint: da:fa:f7:fa:66:84:ec:06:8f:14:50:bd:c7:c2:81:a5:bc:a9:64:57
# SHA256 Fingerprint: ed:f7:eb:bc:a2:7a:2a:38:4d:38:7b:7d:40:10:c6:66:e2:ed:b4:84:3e:4c:29:b4:ae:1d:5b:93:32:e6:b2:4d
-----BEGIN CERTIFICATE-----
MIIFWTCCA0GgAwIBAgIBAjANBgkqhkiG9w0BAQsFADBOMQswCQYDVQQGEwJOTzEd
MBsGA1UECgwUQnV5cGFzcyBBUy05ODMxNjMzMjcxIDAeBgNVBAMMF0J1eXBhc3Mg
Q2xhc3MgMyBSb290IENBMB4XDTEwMTAyNjA4Mjg1OFoXDTQwMTAyNjA4Mjg1OFow
TjELMAkGA1UEBhMCTk8xHTAbBgNVBAoMFEJ1eXBhc3MgQVMtOTgzMTYzMzI3MSAw
HgYDVQQDDBdCdXlwYXNzIENsYXNzIDMgUm9vdCBDQTCCAiIwDQYJKoZIhvcNAQEB
BQADggIPADCCAgoCggIBAKXaCpUWUOOV8l6ddjEGMnqb8RB2uACatVI2zSRHsJ8Y
ZLya9vrVediQYkwiL944PdbgqOkcLNt4EemOaFEVcsfzM4fkoF0LXOBXByow9c3E
N3coTRiR5r/VUv1xLXA+58bEiuPwKAv0dpihi4dVsjoT/Lc+JzeOIuOoTyrvYLs9
tznDDgFHmV0ST9tD+leh7fmdvhFHJlsTmKtdFoqwNxxXnUX/iJY2v7vKB3tvh2PX
0DJq1l1sDPGzbjniazEuOQAnFN44wOwZZoYS6J1yFhNkUsepNxz9gjDthBgd9K5c
/3ATAOux9TN6S9ZV+AWNS2mw9bMoNlwUxFFzTWsL8TQH2xc519woe2v1n/MuwU8X
KhDzzMro6/1rqy6any2CbgTUUgGTLT2G/H783+9CHaZr77kgxve9oKeV/afmiSTY
zIw0bOIjL9kSGiG5VZFvC5F5GQytQIgLcOJ60g7YaEi7ghM5EFjp2CoHxhLbWNvS
O1UQRwUVZ2J+GGOmRj8JDlQyXr8NYnon74Do29lLBlo3WiXQCBJ31G8JUJc9yB3D
34xFMFbG02SrZvPAXpacw8Tvw3xrizp5f7NJzz3iiZ+gMEuFuZyUJHmPfWupRWgP
K9Dx2hzLabjKSWJtyNBjYt1gD1iqj6G8BaVmos8bdrKEZLFMOVLAMLrwjEsCsLa3
AgMBAAGjQjBAMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFEe4zf/lb+74suwv
Tg75JbCOPGvDMA4GA1UdDwEB/wQEAwIBBjANBgkqhkiG9w0BAQsFAAOCAgEAACAj
QTUEkMJAYmDv4jVM1z+s4jSQuKFvdvoWFqRINyzpkMLyPPgKn9iB5btb2iUspKdV
cSQy9sgL8rxq+JOssgfCX5/bzMiKqr5qb+FJEMwx14C7u8jYog5kV+qi9cKpMRXS
IGrs/CIBKM+GuIAeqcwRpTzyFrNHnfzSgCHEy9BHcEGhyoMZCCxt8l13nIoUE9Q2
HJLw5QY33KbmkJs4j1xrG0aGQ0JfPgEHU1RdZX33inOhmlRaHylDFCfChQ+1iHsa
O5S3HWCntZznKWlXWpuTekMwGwPXYshApqr8ZORK15FTAaggiG6cX0S5y2CBNOxv
033aSF/rtJC8LakcC6wc1aJoIIAE1vyxjy+7SjENSoYc6+I2KSb12tjE8nVhz36u
dmNKekBlk4f4HoCMhuWG1o8O/FMsYOgWYRqiPkN7zTlgVGr18okmAWiDSKIz6MkE
kbIRNBE+6tBDGR8Dk5AM/1E9V/RBbuHLoL7ryWPNbczk+DaqaJ3tvV2XcEQNtg41
3OEMXbugUZTLfhbrES+jkkXITHHZvMmZUldGL1DPvTVp9D0VzgalLA8+9oG6lLvD
u79leNKGef9JOxqDDPDeeOzI8k1MGt6CKfjBWtrt7uYnXuhF0J0cUahoq0Tj0Itq
4/g7u9xN12TyUb7mqqta6THuBrxzvxNiCp/HuZc=
-----END CERTIFICATE-----

# Issuer: CN=T-TeleSec GlobalRoot Class 3 O=T-Systems Enterprise Services GmbH OU=T-Systems Trust Center
# Subject: CN=T-TeleSec GlobalRoot Class 3 O=T-Systems Enterprise Services GmbH OU=T-Systems Trust Center
# Label: "T-TeleSec GlobalRoot Class 3"
# Serial: 1
# MD5 Fingerprint: ca:fb:40:a8:4e:39:92:8a:1d:fe:8e:2f:c4:27:ea:ef
# SHA1 Fingerprint: 55:a6:72:3e:cb:f2:ec:cd:c3:23:74:70:19:9d:2a:be:11:e3:81:d1
# SHA256 Fingerprint: fd:73:da:d3:1c:64:4f:f1:b4:3b:ef:0c:cd:da:96:71:0b:9c:d9:87:5e:ca:7e:31:70:7a:f3:e9:6d:52:2b:bd
-----BEGIN CERTIFICATE-----
MIIDwzCCAqugAwIBAgIBATANBgkqhkiG9w0BAQsFADCBgjELMAkGA1UEBhMCREUx
KzApBgNVBAoMIlQtU3lzdGVtcyBFbnRlcnByaXNlIFNlcnZpY2VzIEdtYkgxHzAd
BgNVBAsMFlQtU3lzdGVtcyBUcnVzdCBDZW50ZXIxJTAjBgNVBAMMHFQtVGVsZVNl
YyBHbG9iYWxSb290IENsYXNzIDMwHhcNMDgxMDAxMTAyOTU2WhcNMzMxMDAxMjM1
OTU5WjCBgjELMAkGA1UEBhMCREUxKzApBgNVBAoMIlQtU3lzdGVtcyBFbnRlcnBy
aXNlIFNlcnZpY2VzIEdtYkgxHzAdBgNVBAsMFlQtU3lzdGVtcyBUcnVzdCBDZW50
ZXIxJTAjBgNVBAMMHFQtVGVsZVNlYyBHbG9iYWxSb290IENsYXNzIDMwggEiMA0G
CSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQC9dZPwYiJvJK7genasfb3ZJNW4t/zN
8ELg63iIVl6bmlQdTQyK9tPPcPRStdiTBONGhnFBSivwKixVA9ZIw+A5OO3yXDw/
RLyTPWGrTs0NvvAgJ1gORH8EGoel15YUNpDQSXuhdfsaa3Ox+M6pCSzyU9XDFES4
hqX2iys52qMzVNn6chr3IhUciJFrf2blw2qAsCTz34ZFiP0Zf3WHHx+xGwpzJFu5
ZeAsVMhg02YXP+HMVDNzkQI6pn97djmiH5a2OK61yJN0HZ65tOVgnS9W0eDrXltM
EnAMbEQgqxHY9Bn20pxSN+f6tsIxO0rUFJmtxxr1XV/6B7h8DR/Wgx6zAgMBAAGj
QjBAMA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMB0GA1UdDgQWBBS1
A/d2O2GCahKqGFPrAyGUv/7OyjANBgkqhkiG9w0BAQsFAAOCAQEAVj3vlNW92nOy
WL6ukK2YJ5f+AbGwUgC4TeQbIXQbfsDuXmkqJa9c1h3a0nnJ85cp4IaH3gRZD/FZ
1GSFS5mvJQQeyUapl96Cshtwn5z2r3Ex3XsFpSzTucpH9sry9uetuUg/vBa3wW30
6gmv7PO15wWeph6KU1HWk4HMdJP2udqmJQV0eVp+QD6CSyYRMG7hP0HHRwA11fXT
91Q+gT3aSWqas+8QPebrb9HIIkfLzM8BMZLZGOMivgkeGj5asuRrDFR6fUNOuIml
e9eiPZaGzPImNC1qkp2aGtAw4l1OBLBfiyB+d8E9lYLRRpo7PHi4b6HQDWSieB4p
TpPDpFQUWw==
-----END CERTIFICATE-----

# Issuer: CN=EE Certification Centre Root CA O=AS Sertifitseerimiskeskus
# Subject: CN=EE Certification Centre Root CA O=AS Sertifitseerimiskeskus
# Label: "EE Certification Centre Root CA"
# Serial: 112324828676200291871926431888494945866
# MD5 Fingerprint: 43:5e:88:d4:7d:1a:4a:7e:fd:84:2e:52:eb:01:d4:6f
# SHA1 Fingerprint: c9:a8:b9:e7:55:80:5e:58:e3:53:77:a7:25:eb:af:c3:7b:27:cc:d7
# SHA256 Fingerprint: 3e:84:ba:43:42:90:85:16:e7:75:73:c0:99:2f:09:79:ca:08:4e:46:85:68:1f:f1:95:cc:ba:8a:22:9b:8a:76
-----BEGIN CERTIFICATE-----
MIIEAzCCAuugAwIBAgIQVID5oHPtPwBMyonY43HmSjANBgkqhkiG9w0BAQUFADB1
MQswCQYDVQQGEwJFRTEiMCAGA1UECgwZQVMgU2VydGlmaXRzZWVyaW1pc2tlc2t1
czEoMCYGA1UEAwwfRUUgQ2VydGlmaWNhdGlvbiBDZW50cmUgUm9vdCBDQTEYMBYG
CSqGSIb3DQEJARYJcGtpQHNrLmVlMCIYDzIwMTAxMDMwMTAxMDMwWhgPMjAzMDEy
MTcyMzU5NTlaMHUxCzAJBgNVBAYTAkVFMSIwIAYDVQQKDBlBUyBTZXJ0aWZpdHNl
ZXJpbWlza2Vza3VzMSgwJgYDVQQDDB9FRSBDZXJ0aWZpY2F0aW9uIENlbnRyZSBS
b290IENBMRgwFgYJKoZIhvcNAQkBFglwa2lAc2suZWUwggEiMA0GCSqGSIb3DQEB
AQUAA4IBDwAwggEKAoIBAQDIIMDs4MVLqwd4lfNE7vsLDP90jmG7sWLqI9iroWUy
euuOF0+W2Ap7kaJjbMeMTC55v6kF/GlclY1i+blw7cNRfdCT5mzrMEvhvH2/UpvO
bntl8jixwKIy72KyaOBhU8E2lf/slLo2rpwcpzIP5Xy0xm90/XsY6KxX7QYgSzIw
WFv9zajmofxwvI6Sc9uXp3whrj3B9UiHbCe9nyV0gVWw93X2PaRka9ZP585ArQ/d
MtO8ihJTmMmJ+xAdTX7Nfh9WDSFwhfYggx/2uh8Ej+p3iDXE/+pOoYtNP2MbRMNE
1CV2yreN1x5KZmTNXMWcg+HCCIia7E6j8T4cLNlsHaFLAgMBAAGjgYowgYcwDwYD
VR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMCAQYwHQYDVR0OBBYEFBLyWj7qVhy/
zQas8fElyalL1BSZMEUGA1UdJQQ+MDwGCCsGAQUFBwMCBggrBgEFBQcDAQYIKwYB
BQUHAwMGCCsGAQUFBwMEBggrBgEFBQcDCAYIKwYBBQUHAwkwDQYJKoZIhvcNAQEF
BQADggEBAHv25MANqhlHt01Xo/6tu7Fq1Q+e2+RjxY6hUFaTlrg4wCQiZrxTFGGV
v9DHKpY5P30osxBAIWrEr7BSdxjhlthWXePdNl4dp1BUoMUq5KqMlIpPnTX/dqQG
E5Gion0ARD9V04I8GtVbvFZMIi5GQ4okQC3zErg7cBqklrkar4dBGmoYDQZPxz5u
uSlNDUmJEYcyW+ZLBMjkXOZ0c5RdFpgTlf7727FE5TpwrDdr5rMzcijJs1eg9gIW
iAYLtqZLICjU3j2LrTcFU3T+bsy8QxdxXvnFzBqpYe73dgzzcvRyrc9yAjYHR8/v
GVCJYMzpJJUPwssd8m92kMfMdcGWxZ0=
-----END CERTIFICATE-----

# Issuer: CN=TÜRKTRUST Elektronik Sertifika Hizmet Sağlayıcısı O=TÜRKTRUST Bilgi İletişim ve Bilişim Güvenliği Hizmetleri A.Ş. (c) Aralık 2007
# Subject: CN=TÜRKTRUST Elektronik Sertifika Hizmet Sağlayıcısı O=TÜRKTRUST Bilgi İletişim ve Bilişim Güvenliği Hizmetleri A.Ş. (c) Aralık 2007
# Label: "TURKTRUST Certificate Services Provider Root 2007"
# Serial: 1
# MD5 Fingerprint: 2b:70:20:56:86:82:a0:18:c8:07:53:12:28:70:21:72
# SHA1 Fingerprint: f1:7f:6f:b6:31:dc:99:e3:a3:c8:7f:fe:1c:f1:81:10:88:d9:60:33
# SHA256 Fingerprint: 97:8c:d9:66:f2:fa:a0:7b:a7:aa:95:00:d9:c0:2e:9d:77:f2:cd:ad:a6:ad:6b:a7:4a:f4:b9:1c:66:59:3c:50
-----BEGIN CERTIFICATE-----
MIIEPTCCAyWgAwIBAgIBATANBgkqhkiG9w0BAQUFADCBvzE/MD0GA1UEAww2VMOc
UktUUlVTVCBFbGVrdHJvbmlrIFNlcnRpZmlrYSBIaXptZXQgU2HEn2xhecSxY8Sx
c8SxMQswCQYDVQQGEwJUUjEPMA0GA1UEBwwGQW5rYXJhMV4wXAYDVQQKDFVUw5xS
S1RSVVNUIEJpbGdpIMSwbGV0acWfaW0gdmUgQmlsacWfaW0gR8O8dmVubGnEn2kg
SGl6bWV0bGVyaSBBLsWeLiAoYykgQXJhbMSxayAyMDA3MB4XDTA3MTIyNTE4Mzcx
OVoXDTE3MTIyMjE4MzcxOVowgb8xPzA9BgNVBAMMNlTDnFJLVFJVU1QgRWxla3Ry
b25payBTZXJ0aWZpa2EgSGl6bWV0IFNhxJ9sYXnEsWPEsXPEsTELMAkGA1UEBhMC
VFIxDzANBgNVBAcMBkFua2FyYTFeMFwGA1UECgxVVMOcUktUUlVTVCBCaWxnaSDE
sGxldGnFn2ltIHZlIEJpbGnFn2ltIEfDvHZlbmxpxJ9pIEhpem1ldGxlcmkgQS7F
ni4gKGMpIEFyYWzEsWsgMjAwNzCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoC
ggEBAKu3PgqMyKVYFeaK7yc9SrToJdPNM8Ig3BnuiD9NYvDdE3ePYakqtdTyuTFY
KTsvP2qcb3N2Je40IIDu6rfwxArNK4aUyeNgsURSsloptJGXg9i3phQvKUmi8wUG
+7RP2qFsmmaf8EMJyupyj+sA1zU511YXRxcw9L6/P8JorzZAwan0qafoEGsIiveG
HtyaKhUG9qPw9ODHFNRRf8+0222vR5YXm3dx2KdxnSQM9pQ/hTEST7ruToK4uT6P
IzdezKKqdfcYbwnTrqdUKDT74eA7YH2gvnmJhsifLfkKS8RQouf9eRbHegsYz85M
733WB2+Y8a+xwXrXgTW4qhe04MsCAwEAAaNCMEAwHQYDVR0OBBYEFCnFkKslrxHk
Yb+j/4hhkeYO/pyBMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/MA0G
CSqGSIb3DQEBBQUAA4IBAQAQDdr4Ouwo0RSVgrESLFF6QSU2TJ/sPx+EnWVUXKgW
AkD6bho3hO9ynYYKVZ1WKKxmLNA6VpM0ByWtCLCPyA8JWcqdmBzlVPi5RX9ql2+I
aE1KBiY3iAIOtsbWcpnOa3faYjGkVh+uX4132l32iPwa2Z61gfAyuOOI0JzzaqC5
mxRZNTZPz/OOXl0XrRWV2N2y1RVuAE6zS89mlOTgzbUF2mNXi+WzqtvALhyQRNsa
XRik7r4EW5nVcV9VZWRi1aKbBFmGyGJ353yCRWo9F7/snXUMrqNvWtMvmDb08PUZ
qxFdyKbjKlhqQgnDvZImZjINXQhVdP+MmNAKpoRq0Tl9
-----END CERTIFICATE-----

# Issuer: CN=D-TRUST Root Class 3 CA 2 2009 O=D-Trust GmbH
# Subject: CN=D-TRUST Root Class 3 CA 2 2009 O=D-Trust GmbH
# Label: "D-TRUST Root Class 3 CA 2 2009"
# Serial: 623603
# MD5 Fingerprint: cd:e0:25:69:8d:47:ac:9c:89:35:90:f7:fd:51:3d:2f
# SHA1 Fingerprint: 58:e8:ab:b0:36:15:33:fb:80:f7:9b:1b:6d:29:d3:ff:8d:5f:00:f0
# SHA256 Fingerprint: 49:e7:a4:42:ac:f0:ea:62:87:05:00:54:b5:25:64:b6:50:e4:f4:9e:42:e3:48:d6:aa:38:e0:39:e9:57:b1:c1
-----BEGIN CERTIFICATE-----
MIIEMzCCAxugAwIBAgIDCYPzMA0GCSqGSIb3DQEBCwUAME0xCzAJBgNVBAYTAkRF
MRUwEwYDVQQKDAxELVRydXN0IEdtYkgxJzAlBgNVBAMMHkQtVFJVU1QgUm9vdCBD
bGFzcyAzIENBIDIgMjAwOTAeFw0wOTExMDUwODM1NThaFw0yOTExMDUwODM1NTha
ME0xCzAJBgNVBAYTAkRFMRUwEwYDVQQKDAxELVRydXN0IEdtYkgxJzAlBgNVBAMM
HkQtVFJVU1QgUm9vdCBDbGFzcyAzIENBIDIgMjAwOTCCASIwDQYJKoZIhvcNAQEB
BQADggEPADCCAQoCggEBANOySs96R+91myP6Oi/WUEWJNTrGa9v+2wBoqOADER03
UAifTUpolDWzU9GUY6cgVq/eUXjsKj3zSEhQPgrfRlWLJ23DEE0NkVJD2IfgXU42
tSHKXzlABF9bfsyjxiupQB7ZNoTWSPOSHjRGICTBpFGOShrvUD9pXRl/RcPHAY9R
ySPocq60vFYJfxLLHLGvKZAKyVXMD9O0Gu1HNVpK7ZxzBCHQqr0ME7UAyiZsxGsM
lFqVlNpQmvH/pStmMaTJOKDfHR+4CS7zp+hnUquVH+BGPtikw8paxTGA6Eian5Rp
/hnd2HN8gcqW3o7tszIFZYQ05ub9VxC1X3a/L7AQDcUCAwEAAaOCARowggEWMA8G
A1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFP3aFMSfMN4hvR5COfyrYyNJ4PGEMA4G
A1UdDwEB/wQEAwIBBjCB0wYDVR0fBIHLMIHIMIGAoH6gfIZ6bGRhcDovL2RpcmVj
dG9yeS5kLXRydXN0Lm5ldC9DTj1ELVRSVVNUJTIwUm9vdCUyMENsYXNzJTIwMyUy
MENBJTIwMiUyMDIwMDksTz1ELVRydXN0JTIwR21iSCxDPURFP2NlcnRpZmljYXRl
cmV2b2NhdGlvbmxpc3QwQ6BBoD+GPWh0dHA6Ly93d3cuZC10cnVzdC5uZXQvY3Js
L2QtdHJ1c3Rfcm9vdF9jbGFzc18zX2NhXzJfMjAwOS5jcmwwDQYJKoZIhvcNAQEL
BQADggEBAH+X2zDI36ScfSF6gHDOFBJpiBSVYEQBrLLpME+bUMJm2H6NMLVwMeni
acfzcNsgFYbQDfC+rAF1hM5+n02/t2A7nPPKHeJeaNijnZflQGDSNiH+0LS4F9p0
o3/U37CYAqxva2ssJSRyoWXuJVrl5jLn8t+rSfrzkGkj2wTZ51xY/GXUl77M/C4K
zCUqNQT4YJEVdT1B/yMfGchs64JTBKbkTCJNjYy6zltz7GRUUG3RnFX7acM2w4y8
PIWmawomDeCTmGCufsYkl4phX5GOZpIJhzbNi5stPvZR1FDUWSi9g/LMKHtThm3Y
Johw1+qRzT65ysCQblrGXnRl11z+o+I=
-----END CERTIFICATE-----

# Issuer: CN=D-TRUST Root Class 3 CA 2 EV 2009 O=D-Trust GmbH
# Subject: CN=D-TRUST Root Class 3 CA 2 EV 2009 O=D-Trust GmbH
# Label: "D-TRUST Root Class 3 CA 2 EV 2009"
# Serial: 623604
# MD5 Fingerprint: aa:c6:43:2c:5e:2d:cd:c4:34:c0:50:4f:11:02:4f:b6
# SHA1 Fingerprint: 96:c9:1b:0b:95:b4:10:98:42:fa:d0:d8:22:79:fe:60:fa:b9:16:83
# SHA256 Fingerprint: ee:c5:49:6b:98:8c:e9:86:25:b9:34:09:2e:ec:29:08:be:d0:b0:f3:16:c2:d4:73:0c:84:ea:f1:f3:d3:48:81
-----BEGIN CERTIFICATE-----
MIIEQzCCAyugAwIBAgIDCYP0MA0GCSqGSIb3DQEBCwUAMFAxCzAJBgNVBAYTAkRF
MRUwEwYDVQQKDAxELVRydXN0IEdtYkgxKjAoBgNVBAMMIUQtVFJVU1QgUm9vdCBD
bGFzcyAzIENBIDIgRVYgMjAwOTAeFw0wOTExMDUwODUwNDZaFw0yOTExMDUwODUw
NDZaMFAxCzAJBgNVBAYTAkRFMRUwEwYDVQQKDAxELVRydXN0IEdtYkgxKjAoBgNV
BAMMIUQtVFJVU1QgUm9vdCBDbGFzcyAzIENBIDIgRVYgMjAwOTCCASIwDQYJKoZI
hvcNAQEBBQADggEPADCCAQoCggEBAJnxhDRwui+3MKCOvXwEz75ivJn9gpfSegpn
ljgJ9hBOlSJzmY3aFS3nBfwZcyK3jpgAvDw9rKFs+9Z5JUut8Mxk2og+KbgPCdM0
3TP1YtHhzRnp7hhPTFiu4h7WDFsVWtg6uMQYZB7jM7K1iXdODL/ZlGsTl28So/6Z
qQTMFexgaDbtCHu39b+T7WYxg4zGcTSHThfqr4uRjRxWQa4iN1438h3Z0S0NL2lR
p75mpoo6Kr3HGrHhFPC+Oh25z1uxav60sUYgovseO3Dvk5h9jHOW8sXvhXCtKSb8
HgQ+HKDYD8tSg2J87otTlZCpV6LqYQXY+U3EJ/pure3511H3a6UCAwEAAaOCASQw
ggEgMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFNOUikxiEyoZLsyvcop9Ntea
HNxnMA4GA1UdDwEB/wQEAwIBBjCB3QYDVR0fBIHVMIHSMIGHoIGEoIGBhn9sZGFw
Oi8vZGlyZWN0b3J5LmQtdHJ1c3QubmV0L0NOPUQtVFJVU1QlMjBSb290JTIwQ2xh
c3MlMjAzJTIwQ0ElMjAyJTIwRVYlMjAyMDA5LE89RC1UcnVzdCUyMEdtYkgsQz1E
RT9jZXJ0aWZpY2F0ZXJldm9jYXRpb25saXN0MEagRKBChkBodHRwOi8vd3d3LmQt
dHJ1c3QubmV0L2NybC9kLXRydXN0X3Jvb3RfY2xhc3NfM19jYV8yX2V2XzIwMDku
Y3JsMA0GCSqGSIb3DQEBCwUAA4IBAQA07XtaPKSUiO8aEXUHL7P+PPoeUSbrh/Yp
3uDx1MYkCenBz1UbtDDZzhr+BlGmFaQt77JLvyAoJUnRpjZ3NOhk31KxEcdzes05
nsKtjHEh8lprr988TlWvsoRlFIm5d8sqMb7Po23Pb0iUMkZv53GMoKaEGTcH8gNF
CSuGdXzfX2lXANtu2KZyIktQ1HWYVt+3GP9DQ1CuekR78HlR10M9p9OB0/DJT7na
xpeG0ILD5EJt/rDiZE4OJudANCa1CInXCGNjOCd1HjPqbqjdn5lPdE2BiYBL3ZqX
KVwvvoFBuYz/6n1gBp7N1z3TLqMVvKjmJuVvw9y4AyHqnxbxLFS1
-----END CERTIFICATE-----

# Issuer: CN=Autoridad de Certificacion Raiz del Estado Venezolano O=Sistema Nacional de Certificacion Electronica OU=Superintendencia de Servicios de Certificacion Electronica
# Subject: CN=PSCProcert O=Sistema Nacional de Certificacion Electronica OU=Proveedor de Certificados PROCERT
# Label: "PSCProcert"
# Serial: 11
# MD5 Fingerprint: e6:24:e9:12:01:ae:0c:de:8e:85:c4:ce:a3:12:dd:ec
# SHA1 Fingerprint: 70:c1:8d:74:b4:28:81:0a:e4:fd:a5:75:d7:01:9f:99:b0:3d:50:74
# SHA256 Fingerprint: 3c:fc:3c:14:d1:f6:84:ff:17:e3:8c:43:ca:44:0c:00:b9:67:ec:93:3e:8b:fe:06:4c:a1:d7:2c:90:f2:ad:b0
-----BEGIN CERTIFICATE-----
MIIJhjCCB26gAwIBAgIBCzANBgkqhkiG9w0BAQsFADCCAR4xPjA8BgNVBAMTNUF1
dG9yaWRhZCBkZSBDZXJ0aWZpY2FjaW9uIFJhaXogZGVsIEVzdGFkbyBWZW5lem9s
YW5vMQswCQYDVQQGEwJWRTEQMA4GA1UEBxMHQ2FyYWNhczEZMBcGA1UECBMQRGlz
dHJpdG8gQ2FwaXRhbDE2MDQGA1UEChMtU2lzdGVtYSBOYWNpb25hbCBkZSBDZXJ0
aWZpY2FjaW9uIEVsZWN0cm9uaWNhMUMwQQYDVQQLEzpTdXBlcmludGVuZGVuY2lh
IGRlIFNlcnZpY2lvcyBkZSBDZXJ0aWZpY2FjaW9uIEVsZWN0cm9uaWNhMSUwIwYJ
KoZIhvcNAQkBFhZhY3JhaXpAc3VzY2VydGUuZ29iLnZlMB4XDTEwMTIyODE2NTEw
MFoXDTIwMTIyNTIzNTk1OVowgdExJjAkBgkqhkiG9w0BCQEWF2NvbnRhY3RvQHBy
b2NlcnQubmV0LnZlMQ8wDQYDVQQHEwZDaGFjYW8xEDAOBgNVBAgTB01pcmFuZGEx
KjAoBgNVBAsTIVByb3ZlZWRvciBkZSBDZXJ0aWZpY2Fkb3MgUFJPQ0VSVDE2MDQG
A1UEChMtU2lzdGVtYSBOYWNpb25hbCBkZSBDZXJ0aWZpY2FjaW9uIEVsZWN0cm9u
aWNhMQswCQYDVQQGEwJWRTETMBEGA1UEAxMKUFNDUHJvY2VydDCCAiIwDQYJKoZI
hvcNAQEBBQADggIPADCCAgoCggIBANW39KOUM6FGqVVhSQ2oh3NekS1wwQYalNo9
7BVCwfWMrmoX8Yqt/ICV6oNEolt6Vc5Pp6XVurgfoCfAUFM+jbnADrgV3NZs+J74
BCXfgI8Qhd19L3uA3VcAZCP4bsm+lU/hdezgfl6VzbHvvnpC2Mks0+saGiKLt38G
ieU89RLAu9MLmV+QfI4tL3czkkohRqipCKzx9hEC2ZUWno0vluYC3XXCFCpa1sl9
JcLB/KpnheLsvtF8PPqv1W7/U0HU9TI4seJfxPmOEO8GqQKJ/+MMbpfg353bIdD0
PghpbNjU5Db4g7ayNo+c7zo3Fn2/omnXO1ty0K+qP1xmk6wKImG20qCZyFSTXai2
0b1dCl53lKItwIKOvMoDKjSuc/HUtQy9vmebVOvh+qBa7Dh+PsHMosdEMXXqP+UH
0quhJZb25uSgXTcYOWEAM11G1ADEtMo88aKjPvM6/2kwLkDd9p+cJsmWN63nOaK/
6mnbVSKVUyqUtd+tFjiBdWbjxywbk5yqjKPK2Ww8F22c3HxT4CAnQzb5EuE8XL1m
v6JpIzi4mWCZDlZTOpx+FIywBm/xhnaQr/2v/pDGj59/i5IjnOcVdo/Vi5QTcmn7
K2FjiO/mpF7moxdqWEfLcU8UC17IAggmosvpr2uKGcfLFFb14dq12fy/czja+eev
bqQ34gcnAgMBAAGjggMXMIIDEzASBgNVHRMBAf8ECDAGAQH/AgEBMDcGA1UdEgQw
MC6CD3N1c2NlcnRlLmdvYi52ZaAbBgVghl4CAqASDBBSSUYtRy0yMDAwNDAzNi0w
MB0GA1UdDgQWBBRBDxk4qpl/Qguk1yeYVKIXTC1RVDCCAVAGA1UdIwSCAUcwggFD
gBStuyIdxuDSAaj9dlBSk+2YwU2u06GCASakggEiMIIBHjE+MDwGA1UEAxM1QXV0
b3JpZGFkIGRlIENlcnRpZmljYWNpb24gUmFpeiBkZWwgRXN0YWRvIFZlbmV6b2xh
bm8xCzAJBgNVBAYTAlZFMRAwDgYDVQQHEwdDYXJhY2FzMRkwFwYDVQQIExBEaXN0
cml0byBDYXBpdGFsMTYwNAYDVQQKEy1TaXN0ZW1hIE5hY2lvbmFsIGRlIENlcnRp
ZmljYWNpb24gRWxlY3Ryb25pY2ExQzBBBgNVBAsTOlN1cGVyaW50ZW5kZW5jaWEg
ZGUgU2VydmljaW9zIGRlIENlcnRpZmljYWNpb24gRWxlY3Ryb25pY2ExJTAjBgkq
hkiG9w0BCQEWFmFjcmFpekBzdXNjZXJ0ZS5nb2IudmWCAQowDgYDVR0PAQH/BAQD
AgEGME0GA1UdEQRGMESCDnByb2NlcnQubmV0LnZloBUGBWCGXgIBoAwMClBTQy0w
MDAwMDKgGwYFYIZeAgKgEgwQUklGLUotMzE2MzUzNzMtNzB2BgNVHR8EbzBtMEag
RKBChkBodHRwOi8vd3d3LnN1c2NlcnRlLmdvYi52ZS9sY3IvQ0VSVElGSUNBRE8t
UkFJWi1TSEEzODRDUkxERVIuY3JsMCOgIaAfhh1sZGFwOi8vYWNyYWl6LnN1c2Nl
cnRlLmdvYi52ZTA3BggrBgEFBQcBAQQrMCkwJwYIKwYBBQUHMAGGG2h0dHA6Ly9v
Y3NwLnN1c2NlcnRlLmdvYi52ZTBBBgNVHSAEOjA4MDYGBmCGXgMBAjAsMCoGCCsG
AQUFBwIBFh5odHRwOi8vd3d3LnN1c2NlcnRlLmdvYi52ZS9kcGMwDQYJKoZIhvcN
AQELBQADggIBACtZ6yKZu4SqT96QxtGGcSOeSwORR3C7wJJg7ODU523G0+1ng3dS
1fLld6c2suNUvtm7CpsR72H0xpkzmfWvADmNg7+mvTV+LFwxNG9s2/NkAZiqlCxB
3RWGymspThbASfzXg0gTB1GEMVKIu4YXx2sviiCtxQuPcD4quxtxj7mkoP3Yldmv
Wb8lK5jpY5MvYB7Eqvh39YtsL+1+LrVPQA3uvFd359m21D+VJzog1eWuq2w1n8Gh
HVnchIHuTQfiSLaeS5UtQbHh6N5+LwUeaO6/u5BlOsju6rEYNxxik6SgMexxbJHm
pHmJWhSnFFAFTKQAVzAswbVhltw+HoSvOULP5dAssSS830DD7X9jSr3hTxJkhpXz
sOfIt+FTvZLm8wyWuevo5pLtp4EJFAv8lXrPj9Y0TzYS3F7RNHXGRoAvlQSMx4bE
qCaJqD8Zm4G7UaRKhqsLEQ+xrmNTbSjq3TNWOByyrYDT13K9mmyZY+gAu0F2Bbdb
mRiKw7gSXFbPVgx96OLP7bx0R/vu0xdOIk9W/1DzLuY5poLWccret9W6aAjtmcz9
opLLabid+Qqkpj5PkygqYWwHJgD/ll9ohri4zspV4KuxPX+Y1zMOWj3YeMLEYC/H
YvBhkdI4sPaeVdtAgAUSM84dkpvRabP/v/GSCmE1P93+hvS84Bpxs2Km
-----END CERTIFICATE-----

# Issuer: CN=China Internet Network Information Center EV Certificates Root O=China Internet Network Information Center
# Subject: CN=China Internet Network Information Center EV Certificates Root O=China Internet Network Information Center
# Label: "China Internet Network Information Center EV Certificates Root"
# Serial: 1218379777
# MD5 Fingerprint: 55:5d:63:00:97:bd:6a:97:f5:67:ab:4b:fb:6e:63:15
# SHA1 Fingerprint: 4f:99:aa:93:fb:2b:d1:37:26:a1:99:4a:ce:7f:f0:05:f2:93:5d:1e
# SHA256 Fingerprint: 1c:01:c6:f4:db:b2:fe:fc:22:55:8b:2b:ca:32:56:3f:49:84:4a:cf:c3:2b:7b:e4:b0:ff:59:9f:9e:8c:7a:f7
-----BEGIN CERTIFICATE-----
MIID9zCCAt+gAwIBAgIESJ8AATANBgkqhkiG9w0BAQUFADCBijELMAkGA1UEBhMC
Q04xMjAwBgNVBAoMKUNoaW5hIEludGVybmV0IE5ldHdvcmsgSW5mb3JtYXRpb24g
Q2VudGVyMUcwRQYDVQQDDD5DaGluYSBJbnRlcm5ldCBOZXR3b3JrIEluZm9ybWF0
aW9uIENlbnRlciBFViBDZXJ0aWZpY2F0ZXMgUm9vdDAeFw0xMDA4MzEwNzExMjVa
Fw0zMDA4MzEwNzExMjVaMIGKMQswCQYDVQQGEwJDTjEyMDAGA1UECgwpQ2hpbmEg
SW50ZXJuZXQgTmV0d29yayBJbmZvcm1hdGlvbiBDZW50ZXIxRzBFBgNVBAMMPkNo
aW5hIEludGVybmV0IE5ldHdvcmsgSW5mb3JtYXRpb24gQ2VudGVyIEVWIENlcnRp
ZmljYXRlcyBSb290MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAm35z
7r07eKpkQ0H1UN+U8i6yjUqORlTSIRLIOTJCBumD1Z9S7eVnAztUwYyZmczpwA//
DdmEEbK40ctb3B75aDFk4Zv6dOtouSCV98YPjUesWgbdYavi7NifFy2cyjw1l1Vx
zUOFsUcW9SxTgHbP0wBkvUCZ3czY28Sf1hNfQYOL+Q2HklY0bBoQCxfVWhyXWIQ8
hBouXJE0bhlffxdpxWXvayHG1VA6v2G5BY3vbzQ6sm8UY78WO5upKv23KzhmBsUs
4qpnHkWnjQRmQvaPK++IIGmPMowUc9orhpFjIpryp9vOiYurXccUwVswah+xt54u
gQEC7c+WXmPbqOY4twIDAQABo2MwYTAfBgNVHSMEGDAWgBR8cks5x8DbYqVPm6oY
NJKiyoOCWTAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIBBjAdBgNVHQ4E
FgQUfHJLOcfA22KlT5uqGDSSosqDglkwDQYJKoZIhvcNAQEFBQADggEBACrDx0M3
j92tpLIM7twUbY8opJhJywyA6vPtI2Z1fcXTIWd50XPFtQO3WKwMVC/GVhMPMdoG
52U7HW8228gd+f2ABsqjPWYWqJ1MFn3AlUa1UeTiH9fqBk1jjZaM7+czV0I664zB
echNdn3e9rG3geCg+aF4RhcaVpjwTj2rHO3sOdwHSPdj/gauwqRcalsyiMXHM4Ws
ZkJHwlgkmeHlPuV1LI5D1l08eB6olYIpUNHRFrrvwb562bTYzB5MRuF3sTGrvSrI
zo9uoV1/A3U05K2JRVRevq4opbs/eHnrc7MKDf2+yfdWrPa37S+bISnHOLaVxATy
wy39FCqQmbkHzJ8=
-----END CERTIFICATE-----

# Issuer: CN=Swisscom Root CA 2 O=Swisscom OU=Digital Certificate Services
# Subject: CN=Swisscom Root CA 2 O=Swisscom OU=Digital Certificate Services
# Label: "Swisscom Root CA 2"
# Serial: 40698052477090394928831521023204026294
# MD5 Fingerprint: 5b:04:69:ec:a5:83:94:63:18:a7:86:d0:e4:f2:6e:19
# SHA1 Fingerprint: 77:47:4f:c6:30:e4:0f:4c:47:64:3f:84:ba:b8:c6:95:4a:8a:41:ec
# SHA256 Fingerprint: f0:9b:12:2c:71:14:f4:a0:9b:d4:ea:4f:4a:99:d5:58:b4:6e:4c:25:cd:81:14:0d:29:c0:56:13:91:4c:38:41
-----BEGIN CERTIFICATE-----
MIIF2TCCA8GgAwIBAgIQHp4o6Ejy5e/DfEoeWhhntjANBgkqhkiG9w0BAQsFADBk
MQswCQYDVQQGEwJjaDERMA8GA1UEChMIU3dpc3Njb20xJTAjBgNVBAsTHERpZ2l0
YWwgQ2VydGlmaWNhdGUgU2VydmljZXMxGzAZBgNVBAMTElN3aXNzY29tIFJvb3Qg
Q0EgMjAeFw0xMTA2MjQwODM4MTRaFw0zMTA2MjUwNzM4MTRaMGQxCzAJBgNVBAYT
AmNoMREwDwYDVQQKEwhTd2lzc2NvbTElMCMGA1UECxMcRGlnaXRhbCBDZXJ0aWZp
Y2F0ZSBTZXJ2aWNlczEbMBkGA1UEAxMSU3dpc3Njb20gUm9vdCBDQSAyMIICIjAN
BgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAlUJOhJ1R5tMJ6HJaI2nbeHCOFvEr
jw0DzpPMLgAIe6szjPTpQOYXTKueuEcUMncy3SgM3hhLX3af+Dk7/E6J2HzFZ++r
0rk0X2s682Q2zsKwzxNoysjL67XiPS4h3+os1OD5cJZM/2pYmLcX5BtS5X4HAB1f
2uY+lQS3aYg5oUFgJWFLlTloYhyxCwWJwDaCFCE/rtuh/bxvHGCGtlOUSbkrRsVP
ACu/obvLP+DHVxxX6NZp+MEkUp2IVd3Chy50I9AU/SpHWrumnf2U5NGKpV+GY3aF
y6//SSj8gO1MedK75MDvAe5QQQg1I3ArqRa0jG6F6bYRzzHdUyYb3y1aSgJA/MTA
tukxGggo5WDDH8SQjhBiYEQN7Aq+VRhxLKX0srwVYv8c474d2h5Xszx+zYIdkeNL
6yxSNLCK/RJOlrDrcH+eOfdmQrGrrFLadkBXeyq96G4DsguAhYidDMfCd7Camlf0
uPoTXGiTOmekl9AbmbeGMktg2M7v0Ax/lZ9vh0+Hio5fCHyqW/xavqGRn1V9TrAL
acywlKinh/LTSlDcX3KwFnUey7QYYpqwpzmqm59m2I2mbJYV4+by+PGDYmy7Velh
k6M99bFXi08jsJvllGov34zflVEpYKELKeRcVVi3qPyZ7iVNTA6z00yPhOgpD/0Q
VAKFyPnlw4vP5w8CAwEAAaOBhjCBgzAOBgNVHQ8BAf8EBAMCAYYwHQYDVR0hBBYw
FDASBgdghXQBUwIBBgdghXQBUwIBMBIGA1UdEwEB/wQIMAYBAf8CAQcwHQYDVR0O
BBYEFE0mICKJS9PVpAqhb97iEoHF8TwuMB8GA1UdIwQYMBaAFE0mICKJS9PVpAqh
b97iEoHF8TwuMA0GCSqGSIb3DQEBCwUAA4ICAQAyCrKkG8t9voJXiblqf/P0wS4R
fbgZPnm3qKhyN2abGu2sEzsOv2LwnN+ee6FTSA5BesogpxcbtnjsQJHzQq0Qw1zv
/2BZf82Fo4s9SBwlAjxnffUy6S8w5X2lejjQ82YqZh6NM4OKb3xuqFp1mrjX2lhI
REeoTPpMSQpKwhI3qEAMw8jh0FcNlzKVxzqfl9NX+Ave5XLzo9v/tdhZsnPdTSpx
srpJ9csc1fV5yJmz/MFMdOO0vSk3FQQoHt5FRnDsr7p4DooqzgB53MBfGWcsa0vv
aGgLQ+OswWIJ76bdZWGgr4RVSJFSHMYlkSrQwSIjYVmvRRGFHQEkNI/Ps/8XciAT
woCqISxxOQ7Qj1zB09GOInJGTB2Wrk9xseEFKZZZ9LuedT3PDTcNYtsmjGOpI99n
Bjx8Oto0QuFmtEYE3saWmA9LSHokMnWRn6z3aOkquVVlzl1h0ydw2Df+n7mvoC5W
t6NlUe07qxS/TFED6F+KBZvuim6c779o+sjaC+NCydAXFJy3SuCvkychVSa1ZC+N
8f+mQAWFBVzKBxlcCxMoTFh/wqXvRdpg065lYZ1Tg3TCrvJcwhbtkj6EPnNgiLx2
9CzP0H1907he0ZESEOnN3col49XtmS++dYFLJPlFRpTJKSFTnCZFqhMX5OfNeOI5
wSsSnqaeG8XmDtkx2Q==
-----END CERTIFICATE-----

# Issuer: CN=Swisscom Root EV CA 2 O=Swisscom OU=Digital Certificate Services
# Subject: CN=Swisscom Root EV CA 2 O=Swisscom OU=Digital Certificate Services
# Label: "Swisscom Root EV CA 2"
# Serial: 322973295377129385374608406479535262296
# MD5 Fingerprint: 7b:30:34:9f:dd:0a:4b:6b:35:ca:31:51:28:5d:ae:ec
# SHA1 Fingerprint: e7:a1:90:29:d3:d5:52:dc:0d:0f:c6:92:d3:ea:88:0d:15:2e:1a:6b
# SHA256 Fingerprint: d9:5f:ea:3c:a4:ee:dc:e7:4c:d7:6e:75:fc:6d:1f:f6:2c:44:1f:0f:a8:bc:77:f0:34:b1:9e:5d:b2:58:01:5d
-----BEGIN CERTIFICATE-----
MIIF4DCCA8igAwIBAgIRAPL6ZOJ0Y9ON/RAdBB92ylgwDQYJKoZIhvcNAQELBQAw
ZzELMAkGA1UEBhMCY2gxETAPBgNVBAoTCFN3aXNzY29tMSUwIwYDVQQLExxEaWdp
dGFsIENlcnRpZmljYXRlIFNlcnZpY2VzMR4wHAYDVQQDExVTd2lzc2NvbSBSb290
IEVWIENBIDIwHhcNMTEwNjI0MDk0NTA4WhcNMzEwNjI1MDg0NTA4WjBnMQswCQYD
VQQGEwJjaDERMA8GA1UEChMIU3dpc3Njb20xJTAjBgNVBAsTHERpZ2l0YWwgQ2Vy
dGlmaWNhdGUgU2VydmljZXMxHjAcBgNVBAMTFVN3aXNzY29tIFJvb3QgRVYgQ0Eg
MjCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIBAMT3HS9X6lds93BdY7Bx
UglgRCgzo3pOCvrY6myLURYaVa5UJsTMRQdBTxB5f3HSek4/OE6zAMaVylvNwSqD
1ycfMQ4jFrclyxy0uYAyXhqdk/HoPGAsp15XGVhRXrwsVgu42O+LgrQ8uMIkqBPH
oCE2G3pXKSinLr9xJZDzRINpUKTk4RtiGZQJo/PDvO/0vezbE53PnUgJUmfANykR
HvvSEaeFGHR55E+FFOtSN+KxRdjMDUN/rhPSays/p8LiqG12W0OfvrSdsyaGOx9/
5fLoZigWJdBLlzin5M8J0TbDC77aO0RYjb7xnglrPvMyxyuHxuxenPaHZa0zKcQv
idm5y8kDnftslFGXEBuGCxobP/YCfnvUxVFkKJ3106yDgYjTdLRZncHrYTNaRdHL
OdAGalNgHa/2+2m8atwBz735j9m9W8E6X47aD0upm50qKGsaCnw8qyIL5XctcfaC
NYGu+HuB5ur+rPQam3Rc6I8k9l2dRsQs0h4rIWqDJ2dVSqTjyDKXZpBy2uPUZC5f
46Fq9mDU5zXNysRojddxyNMkM3OxbPlq4SjbX8Y96L5V5jcb7STZDxmPX2MYWFCB
UWVv8p9+agTnNCRxunZLWB4ZvRVgRaoMEkABnRDixzgHcgplwLa7JSnaFp6LNYth
7eVxV4O1PHGf40+/fh6Bn0GXAgMBAAGjgYYwgYMwDgYDVR0PAQH/BAQDAgGGMB0G
A1UdIQQWMBQwEgYHYIV0AVMCAgYHYIV0AVMCAjASBgNVHRMBAf8ECDAGAQH/AgED
MB0GA1UdDgQWBBRF2aWBbj2ITY1x0kbBbkUe88SAnTAfBgNVHSMEGDAWgBRF2aWB
bj2ITY1x0kbBbkUe88SAnTANBgkqhkiG9w0BAQsFAAOCAgEAlDpzBp9SSzBc1P6x
XCX5145v9Ydkn+0UjrgEjihLj6p7jjm02Vj2e6E1CqGdivdj5eu9OYLU43otb98T
PLr+flaYC/NUn81ETm484T4VvwYmneTwkLbUwp4wLh/vx3rEUMfqe9pQy3omywC0
Wqu1kx+AiYQElY2NfwmTv9SoqORjbdlk5LgpWgi/UOGED1V7XwgiG/W9mR4U9s70
WBCCswo9GcG/W6uqmdjyMb3lOGbcWAXH7WMaLgqXfIeTK7KK4/HsGOV1timH59yL
Gn602MnTihdsfSlEvoqq9X46Lmgxk7lq2prg2+kupYTNHAq4Sgj5nPFhJpiTt3tm
7JFe3VE/23MPrQRYCd0EApUKPtN236YQHoA96M2kZNEzx5LH4k5E4wnJTsJdhw4S
nr8PyQUQ3nqjsTzyP6WqJ3mtMX0f/fwZacXduT98zca0wjAefm6S139hdlqP65VN
vBFuIXxZN5nQBrz5Bm0yFqXZaajh3DyAHmBR3NdUIR7KYndP+tiPsys6DXhyyWhB
WkdKwqPrGtcKqzwyVcgKEZzfdNbwQBUdyLmPtTbFr/giuMod89a2GQ+fYWVq6nTI
fI/DT11lgh/ZDYnadXL77/FHZxOzyNEZiCcmmpl5fx7kLD977vHeTYuWl8PVP3wb
I+2ksx0WckNLIOFZfsLorSa/ovc=
-----END CERTIFICATE-----

# Issuer: CN=CA Disig Root R1 O=Disig a.s.
# Subject: CN=CA Disig Root R1 O=Disig a.s.
# Label: "CA Disig Root R1"
# Serial: 14052245610670616104
# MD5 Fingerprint: be:ec:11:93:9a:f5:69:21:bc:d7:c1:c0:67:89:cc:2a
# SHA1 Fingerprint: 8e:1c:74:f8:a6:20:b9:e5:8a:f4:61:fa:ec:2b:47:56:51:1a:52:c6
# SHA256 Fingerprint: f9:6f:23:f4:c3:e7:9c:07:7a:46:98:8d:5a:f5:90:06:76:a0:f0:39:cb:64:5d:d1:75:49:b2:16:c8:24:40:ce
-----BEGIN CERTIFICATE-----
MIIFaTCCA1GgAwIBAgIJAMMDmu5QkG4oMA0GCSqGSIb3DQEBBQUAMFIxCzAJBgNV
BAYTAlNLMRMwEQYDVQQHEwpCcmF0aXNsYXZhMRMwEQYDVQQKEwpEaXNpZyBhLnMu
MRkwFwYDVQQDExBDQSBEaXNpZyBSb290IFIxMB4XDTEyMDcxOTA5MDY1NloXDTQy
MDcxOTA5MDY1NlowUjELMAkGA1UEBhMCU0sxEzARBgNVBAcTCkJyYXRpc2xhdmEx
EzARBgNVBAoTCkRpc2lnIGEucy4xGTAXBgNVBAMTEENBIERpc2lnIFJvb3QgUjEw
ggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQCqw3j33Jijp1pedxiy3QRk
D2P9m5YJgNXoqqXinCaUOuiZc4yd39ffg/N4T0Dhf9Kn0uXKE5Pn7cZ3Xza1lK/o
OI7bm+V8u8yN63Vz4STN5qctGS7Y1oprFOsIYgrY3LMATcMjfF9DCCMyEtztDK3A
fQ+lekLZWnDZv6fXARz2m6uOt0qGeKAeVjGu74IKgEH3G8muqzIm1Cxr7X1r5OJe
IgpFy4QxTaz+29FHuvlglzmxZcfe+5nkCiKxLU3lSCZpq+Kq8/v8kiky6bM+TR8n
oc2OuRf7JT7JbvN32g0S9l3HuzYQ1VTW8+DiR0jm3hTaYVKvJrT1cU/J19IG32PK
/yHoWQbgCNWEFVP3Q+V8xaCJmGtzxmjOZd69fwX3se72V6FglcXM6pM6vpmumwKj
rckWtc7dXpl4fho5frLABaTAgqWjR56M6ly2vGfb5ipN0gTco65F97yLnByn1tUD
3AjLLhbKXEAz6GfDLuemROoRRRw1ZS0eRWEkG4IupZ0zXWX4Qfkuy5Q/H6MMMSRE
7cderVC6xkGbrPAXZcD4XW9boAo0PO7X6oifmPmvTiT6l7Jkdtqr9O3jw2Dv1fkC
yC2fg69naQanMVXVz0tv/wQFx1isXxYb5dKj6zHbHzMVTdDypVP1y+E9Tmgt2BLd
qvLmTZtJ5cUoobqwWsagtQIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1Ud
DwEB/wQEAwIBBjAdBgNVHQ4EFgQUiQq0OJMa5qvum5EY+fU8PjXQ04IwDQYJKoZI
hvcNAQEFBQADggIBADKL9p1Kyb4U5YysOMo6CdQbzoaz3evUuii+Eq5FLAR0rBNR
xVgYZk2C2tXck8An4b58n1KeElb21Zyp9HWc+jcSjxyT7Ff+Bw+r1RL3D65hXlaA
SfX8MPWbTx9BLxyE04nH4toCdu0Jz2zBuByDHBb6lM19oMgY0sidbvW9adRtPTXo
HqJPYNcHKfyyo6SdbhWSVhlMCrDpfNIZTUJG7L399ldb3Zh+pE3McgODWF3vkzpB
emOqfDqo9ayk0d2iLbYq/J8BjuIQscTK5GfbVSUZP/3oNn6z4eGBrxEWi1CXYBmC
AMBrTXO40RMHPuq2MU/wQppt4hF05ZSsjYSVPCGvxdpHyN85YmLLW1AL14FABZyb
7bq2ix4Eb5YgOe2kfSnbSM6C3NQCjR0EMVrHS/BsYVLXtFHCgWzN4funodKSds+x
DzdYpPJScWc/DIh4gInByLUfkmO+p3qKViwaqKactV2zY9ATIKHrkWzQjX2v3wvk
F7mGnjixlAxYjOBVqjtjbZqJYLhkKpLGN/R+Q0O3c+gB53+XD9fyexn9GtePyfqF
a3qdnom2piiZk4hA9z7NUaPK6u95RyG1/jLix8NRb76AdPCkwzryT+lf3xkK8jsT
Q6wxpLPn6/wY1gGp8yqPNg7rtLG8t0zJa7+h89n07eLw4+1knj0vllJPgFOL
-----END CERTIFICATE-----

# Issuer: CN=CA Disig Root R2 O=Disig a.s.
# Subject: CN=CA Disig Root R2 O=Disig a.s.
# Label: "CA Disig Root R2"
# Serial: 10572350602393338211
# MD5 Fingerprint: 26:01:fb:d8:27:a7:17:9a:45:54:38:1a:43:01:3b:03
# SHA1 Fingerprint: b5:61:eb:ea:a4:de:e4:25:4b:69:1a:98:a5:57:47:c2:34:c7:d9:71
# SHA256 Fingerprint: e2:3d:4a:03:6d:7b:70:e9:f5:95:b1:42:20:79:d2:b9:1e:df:bb:1f:b6:51:a0:63:3e:aa:8a:9d:c5:f8:07:03
-----BEGIN CERTIFICATE-----
MIIFaTCCA1GgAwIBAgIJAJK4iNuwisFjMA0GCSqGSIb3DQEBCwUAMFIxCzAJBgNV
BAYTAlNLMRMwEQYDVQQHEwpCcmF0aXNsYXZhMRMwEQYDVQQKEwpEaXNpZyBhLnMu
MRkwFwYDVQQDExBDQSBEaXNpZyBSb290IFIyMB4XDTEyMDcxOTA5MTUzMFoXDTQy
MDcxOTA5MTUzMFowUjELMAkGA1UEBhMCU0sxEzARBgNVBAcTCkJyYXRpc2xhdmEx
EzARBgNVBAoTCkRpc2lnIGEucy4xGTAXBgNVBAMTEENBIERpc2lnIFJvb3QgUjIw
ggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQCio8QACdaFXS1tFPbCw3Oe
NcJxVX6B+6tGUODBfEl45qt5WDza/3wcn9iXAng+a0EE6UG9vgMsRfYvZNSrXaNH
PWSb6WiaxswbP7q+sos0Ai6YVRn8jG+qX9pMzk0DIaPY0jSTVpbLTAwAFjxfGs3I
x2ymrdMxp7zo5eFm1tL7A7RBZckQrg4FY8aAamkw/dLukO8NJ9+flXP04SXabBbe
QTg06ov80egEFGEtQX6sx3dOy1FU+16SGBsEWmjGycT6txOgmLcRK7fWV8x8nhfR
yyX+hk4kLlYMeE2eARKmK6cBZW58Yh2EhN/qwGu1pSqVg8NTEQxzHQuyRpDRQjrO
QG6Vrf/GlK1ul4SOfW+eioANSW1z4nuSHsPzwfPrLgVv2RvPN3YEyLRa5Beny912
H9AZdugsBbPWnDTYltxhh5EF5EQIM8HauQhl1K6yNg3ruji6DOWbnuuNZt2Zz9aJ
QfYEkoopKW1rOhzndX0CcQ7zwOe9yxndnWCywmZgtrEE7snmhrmaZkCo5xHtgUUD
i/ZnWejBBhG93c+AAk9lQHhcR1DIm+YfgXvkRKhbhZri3lrVx/k6RGZL5DJUfORs
nLMOPReisjQS1n6yqEm70XooQL6iFh/f5DcfEXP7kAplQ6INfPgGAVUzfbANuPT1
rqVCV3w2EYx7XsQDnYx5nQIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1Ud
DwEB/wQEAwIBBjAdBgNVHQ4EFgQUtZn4r7CU9eMg1gqtzk5WpC5uQu0wDQYJKoZI
hvcNAQELBQADggIBACYGXnDnZTPIgm7ZnBc6G3pmsgH2eDtpXi/q/075KMOYKmFM
tCQSin1tERT3nLXK5ryeJ45MGcipvXrA1zYObYVybqjGom32+nNjf7xueQgcnYqf
GopTpti72TVVsRHFqQOzVju5hJMiXn7B9hJSi+osZ7z+Nkz1uM/Rs0mSO9MpDpkb
lvdhuDvEK7Z4bLQjb/D907JedR+Zlais9trhxTF7+9FGs9K8Z7RiVLoJ92Owk6Ka
+elSLotgEqv89WBW7xBci8QaQtyDW2QOy7W81k/BfDxujRNt+3vrMNDcTa/F1bal
TFtxyegxvug4BkihGuLq0t4SOVga/4AOgnXmt8kHbA7v/zjxmHHEt38OFdAlab0i
nSvtBfZGR6ztwPDUO+Ls7pZbkBNOHlY667DvlruWIxG68kOGdGSVyCh13x01utI3
gzhTODY7z2zp+WsO0PsE6E9312UBeIYMej4hYvF/Y3EMyZ9E26gnonW+boE+18Dr
G5gPcFw0sorMwIUY6256s/daoQe/qUKS82Ail+QUoQebTnbAjn39pCXHR+3/H3Os
zMOl6W8KjptlwlCFtaOgUxLMVYdh84GuEEZhvUQhuMI9dM9+JDX6HAcOmz0iyu8x
L4ysEr3vQCj8KWefshNPZiTEUxnpHikV7+ZtsH8tZ/3zbBt1RqPlShfppNcL
-----END CERTIFICATE-----

# Issuer: CN=ACCVRAIZ1 O=ACCV OU=PKIACCV
# Subject: CN=ACCVRAIZ1 O=ACCV OU=PKIACCV
# Label: "ACCVRAIZ1"
# Serial: 6828503384748696800
# MD5 Fingerprint: d0:a0:5a:ee:05:b6:09:94:21:a1:7d:f1:b2:29:82:02
# SHA1 Fingerprint: 93:05:7a:88:15:c6:4f:ce:88:2f:fa:91:16:52:28:78:bc:53:64:17
# SHA256 Fingerprint: 9a:6e:c0:12:e1:a7:da:9d:be:34:19:4d:47:8a:d7:c0:db:18:22:fb:07:1d:f1:29:81:49:6e:d1:04:38:41:13
-----BEGIN CERTIFICATE-----
MIIH0zCCBbugAwIBAgIIXsO3pkN/pOAwDQYJKoZIhvcNAQEFBQAwQjESMBAGA1UE
AwwJQUNDVlJBSVoxMRAwDgYDVQQLDAdQS0lBQ0NWMQ0wCwYDVQQKDARBQ0NWMQsw
CQYDVQQGEwJFUzAeFw0xMTA1MDUwOTM3MzdaFw0zMDEyMzEwOTM3MzdaMEIxEjAQ
BgNVBAMMCUFDQ1ZSQUlaMTEQMA4GA1UECwwHUEtJQUNDVjENMAsGA1UECgwEQUND
VjELMAkGA1UEBhMCRVMwggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQCb
qau/YUqXry+XZpp0X9DZlv3P4uRm7x8fRzPCRKPfmt4ftVTdFXxpNRFvu8gMjmoY
HtiP2Ra8EEg2XPBjs5BaXCQ316PWywlxufEBcoSwfdtNgM3802/J+Nq2DoLSRYWo
G2ioPej0RGy9ocLLA76MPhMAhN9KSMDjIgro6TenGEyxCQ0jVn8ETdkXhBilyNpA
lHPrzg5XPAOBOp0KoVdDaaxXbXmQeOW1tDvYvEyNKKGno6e6Ak4l0Squ7a4DIrhr
IA8wKFSVf+DuzgpmndFALW4ir50awQUZ0m/A8p/4e7MCQvtQqR0tkw8jq8bBD5L/
0KIV9VMJcRz/RROE5iZe+OCIHAr8Fraocwa48GOEAqDGWuzndN9wrqODJerWx5eH
k6fGioozl2A3ED6XPm4pFdahD9GILBKfb6qkxkLrQaLjlUPTAYVtjrs78yM2x/47
4KElB0iryYl0/wiPgL/AlmXz7uxLaL2diMMxs0Dx6M/2OLuc5NF/1OVYm3z61PMO
m3WR5LpSLhl+0fXNWhn8ugb2+1KoS5kE3fj5tItQo05iifCHJPqDQsGH+tUtKSpa
cXpkatcnYGMN285J9Y0fkIkyF/hzQ7jSWpOGYdbhdQrqeWZ2iE9x6wQl1gpaepPl
uUsXQA+xtrn13k/c4LOsOxFwYIRKQ26ZIMApcQrAZQIDAQABo4ICyzCCAscwfQYI
KwYBBQUHAQEEcTBvMEwGCCsGAQUFBzAChkBodHRwOi8vd3d3LmFjY3YuZXMvZmls
ZWFkbWluL0FyY2hpdm9zL2NlcnRpZmljYWRvcy9yYWl6YWNjdjEuY3J0MB8GCCsG
AQUFBzABhhNodHRwOi8vb2NzcC5hY2N2LmVzMB0GA1UdDgQWBBTSh7Tj3zcnk1X2
VuqB5TbMjB4/vTAPBgNVHRMBAf8EBTADAQH/MB8GA1UdIwQYMBaAFNKHtOPfNyeT
VfZW6oHlNsyMHj+9MIIBcwYDVR0gBIIBajCCAWYwggFiBgRVHSAAMIIBWDCCASIG
CCsGAQUFBwICMIIBFB6CARAAQQB1AHQAbwByAGkAZABhAGQAIABkAGUAIABDAGUA
cgB0AGkAZgBpAGMAYQBjAGkA8wBuACAAUgBhAO0AegAgAGQAZQAgAGwAYQAgAEEA
QwBDAFYAIAAoAEEAZwBlAG4AYwBpAGEAIABkAGUAIABUAGUAYwBuAG8AbABvAGcA
7QBhACAAeQAgAEMAZQByAHQAaQBmAGkAYwBhAGMAaQDzAG4AIABFAGwAZQBjAHQA
cgDzAG4AaQBjAGEALAAgAEMASQBGACAAUQA0ADYAMAAxADEANQA2AEUAKQAuACAA
QwBQAFMAIABlAG4AIABoAHQAdABwADoALwAvAHcAdwB3AC4AYQBjAGMAdgAuAGUA
czAwBggrBgEFBQcCARYkaHR0cDovL3d3dy5hY2N2LmVzL2xlZ2lzbGFjaW9uX2Mu
aHRtMFUGA1UdHwROMEwwSqBIoEaGRGh0dHA6Ly93d3cuYWNjdi5lcy9maWxlYWRt
aW4vQXJjaGl2b3MvY2VydGlmaWNhZG9zL3JhaXphY2N2MV9kZXIuY3JsMA4GA1Ud
DwEB/wQEAwIBBjAXBgNVHREEEDAOgQxhY2N2QGFjY3YuZXMwDQYJKoZIhvcNAQEF
BQADggIBAJcxAp/n/UNnSEQU5CmH7UwoZtCPNdpNYbdKl02125DgBS4OxnnQ8pdp
D70ER9m+27Up2pvZrqmZ1dM8MJP1jaGo/AaNRPTKFpV8M9xii6g3+CfYCS0b78gU
JyCpZET/LtZ1qmxNYEAZSUNUY9rizLpm5U9EelvZaoErQNV/+QEnWCzI7UiRfD+m
AM/EKXMRNt6GGT6d7hmKG9Ww7Y49nCrADdg9ZuM8Db3VlFzi4qc1GwQA9j9ajepD
vV+JHanBsMyZ4k0ACtrJJ1vnE5Bc5PUzolVt3OAJTS+xJlsndQAJxGJ3KQhfnlms
tn6tn1QwIgPBHnFk/vk4CpYY3QIUrCPLBhwepH2NDd4nQeit2hW3sCPdK6jT2iWH
7ehVRE2I9DZ+hJp4rPcOVkkO1jMl1oRQQmwgEh0q1b688nCBpHBgvgW1m54ERL5h
I6zppSSMEYCUWqKiuUnSwdzRp+0xESyeGabu4VXhwOrPDYTkF7eifKXeVSUG7szA
h1xA2syVP1XgNce4hL60Xc16gwFy7ofmXx2utYXGJt/mwZrpHgJHnyqobalbz+xF
d3+YJ5oyXSrjhO7FmGYvliAd3djDJ9ew+f7Zfc3Qn48LFFhRny+Lwzgt3uiP1o2H
pPVWQxaZLPSkVrQ0uGE3ycJYgBugl6H8WY3pEfbRD0tVNEYqi4Y7
-----END CERTIFICATE-----

# Issuer: CN=TWCA Global Root CA O=TAIWAN-CA OU=Root CA
# Subject: CN=TWCA Global Root CA O=TAIWAN-CA OU=Root CA
# Label: "TWCA Global Root CA"
# Serial: 3262
# MD5 Fingerprint: f9:03:7e:cf:e6:9e:3c:73:7a:2a:90:07:69:ff:2b:96
# SHA1 Fingerprint: 9c:bb:48:53:f6:a4:f6:d3:52:a4:e8:32:52:55:60:13:f5:ad:af:65
# SHA256 Fingerprint: 59:76:90:07:f7:68:5d:0f:cd:50:87:2f:9f:95:d5:75:5a:5b:2b:45:7d:81:f3:69:2b:61:0a:98:67:2f:0e:1b
-----BEGIN CERTIFICATE-----
MIIFQTCCAymgAwIBAgICDL4wDQYJKoZIhvcNAQELBQAwUTELMAkGA1UEBhMCVFcx
EjAQBgNVBAoTCVRBSVdBTi1DQTEQMA4GA1UECxMHUm9vdCBDQTEcMBoGA1UEAxMT
VFdDQSBHbG9iYWwgUm9vdCBDQTAeFw0xMjA2MjcwNjI4MzNaFw0zMDEyMzExNTU5
NTlaMFExCzAJBgNVBAYTAlRXMRIwEAYDVQQKEwlUQUlXQU4tQ0ExEDAOBgNVBAsT
B1Jvb3QgQ0ExHDAaBgNVBAMTE1RXQ0EgR2xvYmFsIFJvb3QgQ0EwggIiMA0GCSqG
SIb3DQEBAQUAA4ICDwAwggIKAoICAQCwBdvI64zEbooh745NnHEKH1Jw7W2CnJfF
10xORUnLQEK1EjRsGcJ0pDFfhQKX7EMzClPSnIyOt7h52yvVavKOZsTuKwEHktSz
0ALfUPZVr2YOy+BHYC8rMjk1Ujoog/h7FsYYuGLWRyWRzvAZEk2tY/XTP3VfKfCh
MBwqoJimFb3u/Rk28OKRQ4/6ytYQJ0lM793B8YVwm8rqqFpD/G2Gb3PpN0Wp8DbH
zIh1HrtsBv+baz4X7GGqcXzGHaL3SekVtTzWoWH1EfcFbx39Eb7QMAfCKbAJTibc
46KokWofwpFFiFzlmLhxpRUZyXx1EcxwdE8tmx2RRP1WKKD+u4ZqyPpcC1jcxkt2
yKsi2XMPpfRaAok/T54igu6idFMqPVMnaR1sjjIsZAAmY2E2TqNGtz99sy2sbZCi
laLOz9qC5wc0GZbpuCGqKX6mOL6OKUohZnkfs8O1CWfe1tQHRvMq2uYiN2DLgbYP
oA/pyJV/v1WRBXrPPRXAb94JlAGD1zQbzECl8LibZ9WYkTunhHiVJqRaCPgrdLQA
BDzfuBSO6N+pjWxnkjMdwLfS7JLIvgm/LCkFbwJrnu+8vyq8W8BQj0FwcYeyTbcE
qYSjMq+u7msXi7Kx/mzhkIyIqJdIzshNy/MGz19qCkKxHh53L46g5pIOBvwFItIm
4TFRfTLcDwIDAQABoyMwITAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB
/zANBgkqhkiG9w0BAQsFAAOCAgEAXzSBdu+WHdXltdkCY4QWwa6gcFGn90xHNcgL
1yg9iXHZqjNB6hQbbCEAwGxCGX6faVsgQt+i0trEfJdLjbDorMjupWkEmQqSpqsn
LhpNgb+E1HAerUf+/UqdM+DyucRFCCEK2mlpc3INvjT+lIutwx4116KD7+U4x6WF
H6vPNOw/KP4M8VeGTslV9xzU2KV9Bnpv1d8Q34FOIWWxtuEXeZVFBs5fzNxGiWNo
RI2T9GRwoD2dKAXDOXC4Ynsg/eTb6QihuJ49CcdP+yz4k3ZB3lLg4VfSnQO8d57+
nile98FRYB/e2guyLXW3Q0iT5/Z5xoRdgFlglPx4mI88k1HtQJAH32RjJMtOcQWh
15QaiDLxInQirqWm2BJpTGCjAu4r7NRjkgtevi92a6O2JryPA9gK8kxkRr05YuWW
6zRjESjMlfGt7+/cgFhI6Uu46mWs6fyAtbXIRfmswZ/ZuepiiI7E8UuDEq3mi4TW
nsLrgxifarsbJGAzcMzs9zLzXNl5fe+epP7JI8Mk7hWSsT2RTyaGvWZzJBPqpK5j
wa19hAM8EHiGG3njxPPyBJUgriOCxLM6AGK/5jYk4Ve6xx6QddVfP5VhK8E7zeWz
aGHQRiapIVJpLesux+t3zqY6tQMzT3bR51xUAV3LePTJDL/PEo4XLSNolOer/qmy
KwbQBM0=
-----END CERTIFICATE-----

# Issuer: CN=TeliaSonera Root CA v1 O=TeliaSonera
# Subject: CN=TeliaSonera Root CA v1 O=TeliaSonera
# Label: "TeliaSonera Root CA v1"
# Serial: 199041966741090107964904287217786801558
# MD5 Fingerprint: 37:41:49:1b:18:56:9a:26:f5:ad:c2:66:fb:40:a5:4c
# SHA1 Fingerprint: 43:13:bb:96:f1:d5:86:9b:c1:4e:6a:92:f6:cf:f6:34:69:87:82:37
# SHA256 Fingerprint: dd:69:36:fe:21:f8:f0:77:c1:23:a1:a5:21:c1:22:24:f7:22:55:b7:3e:03:a7:26:06:93:e8:a2:4b:0f:a3:89
-----BEGIN CERTIFICATE-----
MIIFODCCAyCgAwIBAgIRAJW+FqD3LkbxezmCcvqLzZYwDQYJKoZIhvcNAQEFBQAw
NzEUMBIGA1UECgwLVGVsaWFTb25lcmExHzAdBgNVBAMMFlRlbGlhU29uZXJhIFJv
b3QgQ0EgdjEwHhcNMDcxMDE4MTIwMDUwWhcNMzIxMDE4MTIwMDUwWjA3MRQwEgYD
VQQKDAtUZWxpYVNvbmVyYTEfMB0GA1UEAwwWVGVsaWFTb25lcmEgUm9vdCBDQSB2
MTCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIBAMK+6yfwIaPzaSZVfp3F
VRaRXP3vIb9TgHot0pGMYzHw7CTww6XScnwQbfQ3t+XmfHnqjLWCi65ItqwA3GV1
7CpNX8GH9SBlK4GoRz6JI5UwFpB/6FcHSOcZrr9FZ7E3GwYq/t75rH2D+1665I+X
Z75Ljo1kB1c4VWk0Nj0TSO9P4tNmHqTPGrdeNjPUtAa9GAH9d4RQAEX1jF3oI7x+
/jXh7VB7qTCNGdMJjmhnXb88lxhTuylixcpecsHHltTbLaC0H2kD7OriUPEMPPCs
81Mt8Bz17Ww5OXOAFshSsCPN4D7c3TxHoLs1iuKYaIu+5b9y7tL6pe0S7fyYGKkm
dtwoSxAgHNN/Fnct7W+A90m7UwW7XWjH1Mh1Fj+JWov3F0fUTPHSiXk+TT2YqGHe
Oh7S+F4D4MHJHIzTjU3TlTazN19jY5szFPAtJmtTfImMMsJu7D0hADnJoWjiUIMu
sDor8zagrC/kb2HCUQk5PotTubtn2txTuXZZNp1D5SDgPTJghSJRt8czu90VL6R4
pgd7gUY2BIbdeTXHlSw7sKMXNeVzH7RcWe/a6hBle3rQf5+ztCo3O3CLm1u5K7fs
slESl1MpWtTwEhDcTwK7EpIvYtQ/aUN8Ddb8WHUBiJ1YFkveupD/RwGJBmr2X7KQ
arMCpgKIv7NHfirZ1fpoeDVNAgMBAAGjPzA9MA8GA1UdEwEB/wQFMAMBAf8wCwYD
VR0PBAQDAgEGMB0GA1UdDgQWBBTwj1k4ALP1j5qWDNXr+nuqF+gTEjANBgkqhkiG
9w0BAQUFAAOCAgEAvuRcYk4k9AwI//DTDGjkk0kiP0Qnb7tt3oNmzqjMDfz1mgbl
dxSR651Be5kqhOX//CHBXfDkH1e3damhXwIm/9fH907eT/j3HEbAek9ALCI18Bmx
0GtnLLCo4MBANzX2hFxc469CeP6nyQ1Q6g2EdvZR74NTxnr/DlZJLo961gzmJ1Tj
TQpgcmLNkQfWpb/ImWvtxBnmq0wROMVvMeJuScg/doAmAyYp4Db29iBT4xdwNBed
Y2gea+zDTYa4EzAvXUYNR0PVG6pZDrlcjQZIrXSHX8f8MVRBE+LHIQ6e4B4N4cB7
Q4WQxYpYxmUKeFfyxiMPAdkgS94P+5KFdSpcc41teyWRyu5FrgZLAMzTsVlQ2jqI
OylDRl6XK1TOU2+NSueW+r9xDkKLfP0ooNBIytrEgUy7onOTJsjrDNYmiLbAJM+7
vVvrdX3pCI6GMyx5dwlppYn8s3CQh3aP0yK7Qs69cwsgJirQmz1wHiRszYd2qReW
t88NkvuOGKmYSdGe/mBEciG5Ge3C9THxOUiIkCR1VBatzvT4aRRkOfujuLpwQMcn
HL/EVlP6Y2XQ8xwOFvVrhlhNGNTkDY6lnVuR3HYkUD/GKvvZt5y11ubQ2egZixVx
SK236thZiNSQvxaz2emsWWFUyBy6ysHK4bkgTI86k4mloMy/0/Z1pHWWbVY=
-----END CERTIFICATE-----

# Issuer: CN=E-Tugra Certification Authority O=E-Tuğra EBG Bilişim Teknolojileri ve Hizmetleri A.Ş. OU=E-Tugra Sertifikasyon Merkezi
# Subject: CN=E-Tugra Certification Authority O=E-Tuğra EBG Bilişim Teknolojileri ve Hizmetleri A.Ş. OU=E-Tugra Sertifikasyon Merkezi
# Label: "E-Tugra Certification Authority"
# Serial: 7667447206703254355
# MD5 Fingerprint: b8:a1:03:63:b0:bd:21:71:70:8a:6f:13:3a:bb:79:49
# SHA1 Fingerprint: 51:c6:e7:08:49:06:6e:f3:92:d4:5c:a0:0d:6d:a3:62:8f:c3:52:39
# SHA256 Fingerprint: b0:bf:d5:2b:b0:d7:d9:bd:92:bf:5d:4d:c1:3d:a2:55:c0:2c:54:2f:37:83:65:ea:89:39:11:f5:5e:55:f2:3c
-----BEGIN CERTIFICATE-----
MIIGSzCCBDOgAwIBAgIIamg+nFGby1MwDQYJKoZIhvcNAQELBQAwgbIxCzAJBgNV
BAYTAlRSMQ8wDQYDVQQHDAZBbmthcmExQDA+BgNVBAoMN0UtVHXEn3JhIEVCRyBC
aWxpxZ9pbSBUZWtub2xvamlsZXJpIHZlIEhpem1ldGxlcmkgQS7Fni4xJjAkBgNV
BAsMHUUtVHVncmEgU2VydGlmaWthc3lvbiBNZXJrZXppMSgwJgYDVQQDDB9FLVR1
Z3JhIENlcnRpZmljYXRpb24gQXV0aG9yaXR5MB4XDTEzMDMwNTEyMDk0OFoXDTIz
MDMwMzEyMDk0OFowgbIxCzAJBgNVBAYTAlRSMQ8wDQYDVQQHDAZBbmthcmExQDA+
BgNVBAoMN0UtVHXEn3JhIEVCRyBCaWxpxZ9pbSBUZWtub2xvamlsZXJpIHZlIEhp
em1ldGxlcmkgQS7Fni4xJjAkBgNVBAsMHUUtVHVncmEgU2VydGlmaWthc3lvbiBN
ZXJrZXppMSgwJgYDVQQDDB9FLVR1Z3JhIENlcnRpZmljYXRpb24gQXV0aG9yaXR5
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEA4vU/kwVRHoViVF56C/UY
B4Oufq9899SKa6VjQzm5S/fDxmSJPZQuVIBSOTkHS0vdhQd2h8y/L5VMzH2nPbxH
D5hw+IyFHnSOkm0bQNGZDbt1bsipa5rAhDGvykPL6ys06I+XawGb1Q5KCKpbknSF
Q9OArqGIW66z6l7LFpp3RMih9lRozt6Plyu6W0ACDGQXwLWTzeHxE2bODHnv0ZEo
q1+gElIwcxmOj+GMB6LDu0rw6h8VqO4lzKRG+Bsi77MOQ7osJLjFLFzUHPhdZL3D
k14opz8n8Y4e0ypQBaNV2cvnOVPAmJ6MVGKLJrD3fY185MaeZkJVgkfnsliNZvcH
fC425lAcP9tDJMW/hkd5s3kc91r0E+xs+D/iWR+V7kI+ua2oMoVJl0b+SzGPWsut
dEcf6ZG33ygEIqDUD13ieU/qbIWGvaimzuT6w+Gzrt48Ue7LE3wBf4QOXVGUnhMM
ti6lTPk5cDZvlsouDERVxcr6XQKj39ZkjFqzAQqptQpHF//vkUAqjqFGOjGY5RH8
zLtJVor8udBhmm9lbObDyz51Sf6Pp+KJxWfXnUYTTjF2OySznhFlhqt/7x3U+Lzn
rFpct1pHXFXOVbQicVtbC/DP3KBhZOqp12gKY6fgDT+gr9Oq0n7vUaDmUStVkhUX
U8u3Zg5mTPj5dUyQ5xJwx0UCAwEAAaNjMGEwHQYDVR0OBBYEFC7j27JJ0JxUeVz6
Jyr+zE7S6E5UMA8GA1UdEwEB/wQFMAMBAf8wHwYDVR0jBBgwFoAULuPbsknQnFR5
XPonKv7MTtLoTlQwDgYDVR0PAQH/BAQDAgEGMA0GCSqGSIb3DQEBCwUAA4ICAQAF
Nzr0TbdF4kV1JI+2d1LoHNgQk2Xz8lkGpD4eKexd0dCrfOAKkEh47U6YA5n+KGCR
HTAduGN8qOY1tfrTYXbm1gdLymmasoR6d5NFFxWfJNCYExL/u6Au/U5Mh/jOXKqY
GwXgAEZKgoClM4so3O0409/lPun++1ndYYRP0lSWE2ETPo+Aab6TR7U1Q9Jauz1c
77NCR807VRMGsAnb/WP2OogKmW9+4c4bU2pEZiNRCHu8W1Ki/QY3OEBhj0qWuJA3
+GbHeJAAFS6LrVE1Uweoa2iu+U48BybNCAVwzDk/dr2l02cmAYamU9JgO3xDf1WK
vJUawSg5TB9D0pH0clmKuVb8P7Sd2nCcdlqMQ1DujjByTd//SffGqWfZbawCEeI6
FiWnWAjLb1NBnEg4R2gz0dfHj9R0IdTDBZB6/86WiLEVKV0jq9BgoRJP3vQXzTLl
yb/IQ639Lo7xr+L0mPoSHyDYwKcMhcWQ9DstliaxLL5Mq+ux0orJ23gTDx4JnW2P
AJ8C2sH6H3p6CcRK5ogql5+Ji/03X186zjhZhkuvcQu02PJwT58yE+Owp1fl2tpD
y4Q08ijE6m30Ku/Ba3ba+367hTzSU8JNvnHhRdH9I2cNE3X7z2VnIp2usAnRCf8d
NL/+I5c30jn6PQ0GC7TbO6Orb1wdtn7os4I07QZcJA==
-----END CERTIFICATE-----

# Issuer: CN=T-TeleSec GlobalRoot Class 2 O=T-Systems Enterprise Services GmbH OU=T-Systems Trust Center
# Subject: CN=T-TeleSec GlobalRoot Class 2 O=T-Systems Enterprise Services GmbH OU=T-Systems Trust Center
# Label: "T-TeleSec GlobalRoot Class 2"
# Serial: 1
# MD5 Fingerprint: 2b:9b:9e:e4:7b:6c:1f:00:72:1a:cc:c1:77:79:df:6a
# SHA1 Fingerprint: 59:0d:2d:7d:88:4f:40:2e:61:7e:a5:62:32:17:65:cf:17:d8:94:e9
# SHA256 Fingerprint: 91:e2:f5:78:8d:58:10:eb:a7:ba:58:73:7d:e1:54:8a:8e:ca:cd:01:45:98:bc:0b:14:3e:04:1b:17:05:25:52
-----BEGIN CERTIFICATE-----
MIIDwzCCAqugAwIBAgIBATANBgkqhkiG9w0BAQsFADCBgjELMAkGA1UEBhMCREUx
KzApBgNVBAoMIlQtU3lzdGVtcyBFbnRlcnByaXNlIFNlcnZpY2VzIEdtYkgxHzAd
BgNVBAsMFlQtU3lzdGVtcyBUcnVzdCBDZW50ZXIxJTAjBgNVBAMMHFQtVGVsZVNl
YyBHbG9iYWxSb290IENsYXNzIDIwHhcNMDgxMDAxMTA0MDE0WhcNMzMxMDAxMjM1
OTU5WjCBgjELMAkGA1UEBhMCREUxKzApBgNVBAoMIlQtU3lzdGVtcyBFbnRlcnBy
aXNlIFNlcnZpY2VzIEdtYkgxHzAdBgNVBAsMFlQtU3lzdGVtcyBUcnVzdCBDZW50
ZXIxJTAjBgNVBAMMHFQtVGVsZVNlYyBHbG9iYWxSb290IENsYXNzIDIwggEiMA0G
CSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCqX9obX+hzkeXaXPSi5kfl82hVYAUd
AqSzm1nzHoqvNK38DcLZSBnuaY/JIPwhqgcZ7bBcrGXHX+0CfHt8LRvWurmAwhiC
FoT6ZrAIxlQjgeTNuUk/9k9uN0goOA/FvudocP05l03Sx5iRUKrERLMjfTlH6VJi
1hKTXrcxlkIF+3anHqP1wvzpesVsqXFP6st4vGCvx9702cu+fjOlbpSD8DT6Iavq
jnKgP6TeMFvvhk1qlVtDRKgQFRzlAVfFmPHmBiiRqiDFt1MmUUOyCxGVWOHAD3bZ
wI18gfNycJ5v/hqO2V81xrJvNHy+SE/iWjnX2J14np+GPgNeGYtEotXHAgMBAAGj
QjBAMA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMB0GA1UdDgQWBBS/
WSA2AHmgoCJrjNXyYdK4LMuCSjANBgkqhkiG9w0BAQsFAAOCAQEAMQOiYQsfdOhy
NsZt+U2e+iKo4YFWz827n+qrkRk4r6p8FU3ztqONpfSO9kSpp+ghla0+AGIWiPAC
uvxhI+YzmzB6azZie60EI4RYZeLbK4rnJVM3YlNfvNoBYimipidx5joifsFvHZVw
IEoHNN/q/xWA5brXethbdXwFeilHfkCoMRN3zUA7tFFHei4R40cR3p1m0IvVVGb6
g1XqfMIpiRvpb7PO4gWEyS8+eIVibslfwXhjdFjASBgMmTnrpMwatXlajRWc2BQN
9noHV8cigwUtPJslJj0Ys6lDfMjIq2SPDqO/nBudMNva0Bkuqjzx+zOAduTNrRlP
BSeOE6Fuwg==
-----END CERTIFICATE-----

# Issuer: CN=Atos TrustedRoot 2011 O=Atos
# Subject: CN=Atos TrustedRoot 2011 O=Atos
# Label: "Atos TrustedRoot 2011"
# Serial: 6643877497813316402
# MD5 Fingerprint: ae:b9:c4:32:4b:ac:7f:5d:66:cc:77:94:bb:2a:77:56
# SHA1 Fingerprint: 2b:b1:f5:3e:55:0c:1d:c5:f1:d4:e6:b7:6a:46:4b:55:06:02:ac:21
# SHA256 Fingerprint: f3:56:be:a2:44:b7:a9:1e:b3:5d:53:ca:9a:d7:86:4a:ce:01:8e:2d:35:d5:f8:f9:6d:df:68:a6:f4:1a:a4:74
-----BEGIN CERTIFICATE-----
MIIDdzCCAl+gAwIBAgIIXDPLYixfszIwDQYJKoZIhvcNAQELBQAwPDEeMBwGA1UE
AwwVQXRvcyBUcnVzdGVkUm9vdCAyMDExMQ0wCwYDVQQKDARBdG9zMQswCQYDVQQG
EwJERTAeFw0xMTA3MDcxNDU4MzBaFw0zMDEyMzEyMzU5NTlaMDwxHjAcBgNVBAMM
FUF0b3MgVHJ1c3RlZFJvb3QgMjAxMTENMAsGA1UECgwEQXRvczELMAkGA1UEBhMC
REUwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCVhTuXbyo7LjvPpvMp
Nb7PGKw+qtn4TaA+Gke5vJrf8v7MPkfoepbCJI419KkM/IL9bcFyYie96mvr54rM
VD6QUM+A1JX76LWC1BTFtqlVJVfbsVD2sGBkWXppzwO3bw2+yj5vdHLqqjAqc2K+
SZFhyBH+DgMq92og3AIVDV4VavzjgsG1xZ1kCWyjWZgHJ8cblithdHFsQ/H3NYkQ
4J7sVaE3IqKHBAUsR320HLliKWYoyrfhk/WklAOZuXCFteZI6o1Q/NnezG8HDt0L
cp2AMBYHlT8oDv3FdU9T1nSatCQujgKRz3bFmx5VdJx4IbHwLfELn8LVlhgf8FQi
eowHAgMBAAGjfTB7MB0GA1UdDgQWBBSnpQaxLKYJYO7Rl+lwrrw7GWzbITAPBgNV
HRMBAf8EBTADAQH/MB8GA1UdIwQYMBaAFKelBrEspglg7tGX6XCuvDsZbNshMBgG
A1UdIAQRMA8wDQYLKwYBBAGwLQMEAQEwDgYDVR0PAQH/BAQDAgGGMA0GCSqGSIb3
DQEBCwUAA4IBAQAmdzTblEiGKkGdLD4GkGDEjKwLVLgfuXvTBznk+j57sj1O7Z8j
vZfza1zv7v1Apt+hk6EKhqzvINB5Ab149xnYJDE0BAGmuhWawyfc2E8PzBhj/5kP
DpFrdRbhIfzYJsdHt6bPWHJxfrrhTZVHO8mvbaG0weyJ9rQPOLXiZNwlz6bb65pc
maHFCN795trV1lpFDMS3wrUU77QR/w4VtfX128a961qn8FYiqTxlVMYVqL2Gns2D
lmh6cYGJ4Qvh6hEbaAjMaZ7snkGeRDImeuKHCnE96+RapNLbxc3G3mB/ufNPRJLv
KrcYPqcZ2Qt9sTdBQrC6YB3y/gkRsPCHe6ed
-----END CERTIFICATE-----

# Issuer: CN=QuoVadis Root CA 1 G3 O=QuoVadis Limited
# Subject: CN=QuoVadis Root CA 1 G3 O=QuoVadis Limited
# Label: "QuoVadis Root CA 1 G3"
# Serial: 687049649626669250736271037606554624078720034195
# MD5 Fingerprint: a4:bc:5b:3f:fe:37:9a:fa:64:f0:e2:fa:05:3d:0b:ab
# SHA1 Fingerprint: 1b:8e:ea:57:96:29:1a:c9:39:ea:b8:0a:81:1a:73:73:c0:93:79:67
# SHA256 Fingerprint: 8a:86:6f:d1:b2:76:b5:7e:57:8e:92:1c:65:82:8a:2b:ed:58:e9:f2:f2:88:05:41:34:b7:f1:f4:bf:c9:cc:74
-----BEGIN CERTIFICATE-----
MIIFYDCCA0igAwIBAgIUeFhfLq0sGUvjNwc1NBMotZbUZZMwDQYJKoZIhvcNAQEL
BQAwSDELMAkGA1UEBhMCQk0xGTAXBgNVBAoTEFF1b1ZhZGlzIExpbWl0ZWQxHjAc
BgNVBAMTFVF1b1ZhZGlzIFJvb3QgQ0EgMSBHMzAeFw0xMjAxMTIxNzI3NDRaFw00
MjAxMTIxNzI3NDRaMEgxCzAJBgNVBAYTAkJNMRkwFwYDVQQKExBRdW9WYWRpcyBM
aW1pdGVkMR4wHAYDVQQDExVRdW9WYWRpcyBSb290IENBIDEgRzMwggIiMA0GCSqG
SIb3DQEBAQUAA4ICDwAwggIKAoICAQCgvlAQjunybEC0BJyFuTHK3C3kEakEPBtV
wedYMB0ktMPvhd6MLOHBPd+C5k+tR4ds7FtJwUrVu4/sh6x/gpqG7D0DmVIB0jWe
rNrwU8lmPNSsAgHaJNM7qAJGr6Qc4/hzWHa39g6QDbXwz8z6+cZM5cOGMAqNF341
68Xfuw6cwI2H44g4hWf6Pser4BOcBRiYz5P1sZK0/CPTz9XEJ0ngnjybCKOLXSoh
4Pw5qlPafX7PGglTvF0FBM+hSo+LdoINofjSxxR3W5A2B4GbPgb6Ul5jxaYA/qXp
UhtStZI5cgMJYr2wYBZupt0lwgNm3fME0UDiTouG9G/lg6AnhF4EwfWQvTA9xO+o
abw4m6SkltFi2mnAAZauy8RRNOoMqv8hjlmPSlzkYZqn0ukqeI1RPToV7qJZjqlc
3sX5kCLliEVx3ZGZbHqfPT2YfF72vhZooF6uCyP8Wg+qInYtyaEQHeTTRCOQiJ/G
KubX9ZqzWB4vMIkIG1SitZgj7Ah3HJVdYdHLiZxfokqRmu8hqkkWCKi9YSgxyXSt
hfbZxbGL0eUQMk1fiyA6PEkfM4VZDdvLCXVDaXP7a3F98N/ETH3Goy7IlXnLc6KO
Tk0k+17kBL5yG6YnLUlamXrXXAkgt3+UuU/xDRxeiEIbEbfnkduebPRq34wGmAOt
zCjvpUfzUwIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIB
BjAdBgNVHQ4EFgQUo5fW816iEOGrRZ88F2Q87gFwnMwwDQYJKoZIhvcNAQELBQAD
ggIBABj6W3X8PnrHX3fHyt/PX8MSxEBd1DKquGrX1RUVRpgjpeaQWxiZTOOtQqOC
MTaIzen7xASWSIsBx40Bz1szBpZGZnQdT+3Btrm0DWHMY37XLneMlhwqI2hrhVd2
cDMT/uFPpiN3GPoajOi9ZcnPP/TJF9zrx7zABC4tRi9pZsMbj/7sPtPKlL92CiUN
qXsCHKnQO18LwIE6PWThv6ctTr1NxNgpxiIY0MWscgKCP6o6ojoilzHdCGPDdRS5
YCgtW2jgFqlmgiNR9etT2DGbe+m3nUvriBbP+V04ikkwj+3x6xn0dxoxGE1nVGwv
b2X52z3sIexe9PSLymBlVNFxZPT5pqOBMzYzcfCkeF9OrYMh3jRJjehZrJ3ydlo2
8hP0r+AJx2EqbPfgna67hkooby7utHnNkDPDs3b69fBsnQGQ+p6Q9pxyz0fawx/k
NSBT8lTR32GDpgLiJTjehTItXnOQUl1CxM49S+H5GYQd1aJQzEH7QRTDvdbJWqNj
ZgKAvQU6O0ec7AAmTPWIUb+oI38YB7AL7YsmoWTTYUrrXJ/es69nA7Mf3W1daWhp
q1467HxpvMc7hU6eFbm0FU/DlXpY18ls6Wy58yljXrQs8C097Vpl4KlbQMJImYFt
nh8GKjwStIsPm6Ik8KaN1nrgS7ZklmOVhMJKzRwuJIczYOXD
-----END CERTIFICATE-----

# Issuer: CN=QuoVadis Root CA 2 G3 O=QuoVadis Limited
# Subject: CN=QuoVadis Root CA 2 G3 O=QuoVadis Limited
# Label: "QuoVadis Root CA 2 G3"
# Serial: 390156079458959257446133169266079962026824725800
# MD5 Fingerprint: af:0c:86:6e:bf:40:2d:7f:0b:3e:12:50:ba:12:3d:06
# SHA1 Fingerprint: 09:3c:61:f3:8b:8b:dc:7d:55:df:75:38:02:05:00:e1:25:f5:c8:36
# SHA256 Fingerprint: 8f:e4:fb:0a:f9:3a:4d:0d:67:db:0b:eb:b2:3e:37:c7:1b:f3:25:dc:bc:dd:24:0e:a0:4d:af:58:b4:7e:18:40
-----BEGIN CERTIFICATE-----
MIIFYDCCA0igAwIBAgIURFc0JFuBiZs18s64KztbpybwdSgwDQYJKoZIhvcNAQEL
BQAwSDELMAkGA1UEBhMCQk0xGTAXBgNVBAoTEFF1b1ZhZGlzIExpbWl0ZWQxHjAc
BgNVBAMTFVF1b1ZhZGlzIFJvb3QgQ0EgMiBHMzAeFw0xMjAxMTIxODU5MzJaFw00
MjAxMTIxODU5MzJaMEgxCzAJBgNVBAYTAkJNMRkwFwYDVQQKExBRdW9WYWRpcyBM
aW1pdGVkMR4wHAYDVQQDExVRdW9WYWRpcyBSb290IENBIDIgRzMwggIiMA0GCSqG
SIb3DQEBAQUAA4ICDwAwggIKAoICAQChriWyARjcV4g/Ruv5r+LrI3HimtFhZiFf
qq8nUeVuGxbULX1QsFN3vXg6YOJkApt8hpvWGo6t/x8Vf9WVHhLL5hSEBMHfNrMW
n4rjyduYNM7YMxcoRvynyfDStNVNCXJJ+fKH46nafaF9a7I6JaltUkSs+L5u+9ym
c5GQYaYDFCDy54ejiK2toIz/pgslUiXnFgHVy7g1gQyjO/Dh4fxaXc6AcW34Sas+
O7q414AB+6XrW7PFXmAqMaCvN+ggOp+oMiwMzAkd056OXbxMmO7FGmh77FOm6RQ1
o9/NgJ8MSPsc9PG/Srj61YxxSscfrf5BmrODXfKEVu+lV0POKa2Mq1W/xPtbAd0j
IaFYAI7D0GoT7RPjEiuA3GfmlbLNHiJuKvhB1PLKFAeNilUSxmn1uIZoL1NesNKq
IcGY5jDjZ1XHm26sGahVpkUG0CM62+tlXSoREfA7T8pt9DTEceT/AFr2XK4jYIVz
8eQQsSWu1ZK7E8EM4DnatDlXtas1qnIhO4M15zHfeiFuuDIIfR0ykRVKYnLP43eh
vNURG3YBZwjgQQvD6xVu+KQZ2aKrr+InUlYrAoosFCT5v0ICvybIxo/gbjh9Uy3l
7ZizlWNof/k19N+IxWA1ksB8aRxhlRbQ694Lrz4EEEVlWFA4r0jyWbYW8jwNkALG
cC4BrTwV1wIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIB
BjAdBgNVHQ4EFgQU7edvdlq/YOxJW8ald7tyFnGbxD0wDQYJKoZIhvcNAQELBQAD
ggIBAJHfgD9DCX5xwvfrs4iP4VGyvD11+ShdyLyZm3tdquXK4Qr36LLTn91nMX66
AarHakE7kNQIXLJgapDwyM4DYvmL7ftuKtwGTTwpD4kWilhMSA/ohGHqPHKmd+RC
roijQ1h5fq7KpVMNqT1wvSAZYaRsOPxDMuHBR//47PERIjKWnML2W2mWeyAMQ0Ga
W/ZZGYjeVYg3UQt4XAoeo0L9x52ID8DyeAIkVJOviYeIyUqAHerQbj5hLja7NQ4n
lv1mNDthcnPxFlxHBlRJAHpYErAK74X9sbgzdWqTHBLmYF5vHX/JHyPLhGGfHoJE
+V+tYlUkmlKY7VHnoX6XOuYvHxHaU4AshZ6rNRDbIl9qxV6XU/IyAgkwo1jwDQHV
csaxfGl7w/U2Rcxhbl5MlMVerugOXou/983g7aEOGzPuVBj+D77vfoRrQ+NwmNtd
dbINWQeFFSM51vHfqSYP1kjHs6Yi9TM3WpVHn3u6GBVv/9YUZINJ0gpnIdsPNWNg
KCLjsZWDzYWm3S8P52dSbrsvhXz1SnPnxT7AvSESBT/8twNJAlvIJebiVDj1eYeM
HVOyToV7BjjHLPj4sHKNJeV3UvQDHEimUF+IIDBu8oJDqz2XhOdT+yHBTw8imoa4
WSr2Rz0ZiC3oheGe7IUIarFsNMkd7EgrO3jtZsSOeWmD3n+M
-----END CERTIFICATE-----

# Issuer: CN=QuoVadis Root CA 3 G3 O=QuoVadis Limited
# Subject: CN=QuoVadis Root CA 3 G3 O=QuoVadis Limited
# Label: "QuoVadis Root CA 3 G3"
# Serial: 268090761170461462463995952157327242137089239581
# MD5 Fingerprint: df:7d:b9:ad:54:6f:68:a1:df:89:57:03:97:43:b0:d7
# SHA1 Fingerprint: 48:12:bd:92:3c:a8:c4:39:06:e7:30:6d:27:96:e6:a4:cf:22:2e:7d
# SHA256 Fingerprint: 88:ef:81:de:20:2e:b0:18:45:2e:43:f8:64:72:5c:ea:5f:bd:1f:c2:d9:d2:05:73:07:09:c5:d8:b8:69:0f:46
-----BEGIN CERTIFICATE-----
MIIFYDCCA0igAwIBAgIULvWbAiin23r/1aOp7r0DoM8Sah0wDQYJKoZIhvcNAQEL
BQAwSDELMAkGA1UEBhMCQk0xGTAXBgNVBAoTEFF1b1ZhZGlzIExpbWl0ZWQxHjAc
BgNVBAMTFVF1b1ZhZGlzIFJvb3QgQ0EgMyBHMzAeFw0xMjAxMTIyMDI2MzJaFw00
MjAxMTIyMDI2MzJaMEgxCzAJBgNVBAYTAkJNMRkwFwYDVQQKExBRdW9WYWRpcyBM
aW1pdGVkMR4wHAYDVQQDExVRdW9WYWRpcyBSb290IENBIDMgRzMwggIiMA0GCSqG
SIb3DQEBAQUAA4ICDwAwggIKAoICAQCzyw4QZ47qFJenMioKVjZ/aEzHs286IxSR
/xl/pcqs7rN2nXrpixurazHb+gtTTK/FpRp5PIpM/6zfJd5O2YIyC0TeytuMrKNu
FoM7pmRLMon7FhY4futD4tN0SsJiCnMK3UmzV9KwCoWdcTzeo8vAMvMBOSBDGzXR
U7Ox7sWTaYI+FrUoRqHe6okJ7UO4BUaKhvVZR74bbwEhELn9qdIoyhA5CcoTNs+c
ra1AdHkrAj80//ogaX3T7mH1urPnMNA3I4ZyYUUpSFlob3emLoG+B01vr87ERROR
FHAGjx+f+IdpsQ7vw4kZ6+ocYfx6bIrc1gMLnia6Et3UVDmrJqMz6nWB2i3ND0/k
A9HvFZcba5DFApCTZgIhsUfei5pKgLlVj7WiL8DWM2fafsSntARE60f75li59wzw
eyuxwHApw0BiLTtIadwjPEjrewl5qW3aqDCYz4ByA4imW0aucnl8CAMhZa634Ryl
sSqiMd5mBPfAdOhx3v89WcyWJhKLhZVXGqtrdQtEPREoPHtht+KPZ0/l7DxMYIBp
VzgeAVuNVejH38DMdyM0SXV89pgR6y3e7UEuFAUCf+D+IOs15xGsIs5XPd7JMG0Q
A4XN8f+MFrXBsj6IbGB/kE+V9/YtrQE5BwT6dYB9v0lQ7e/JxHwc64B+27bQ3RP+
ydOc17KXqQIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIB
BjAdBgNVHQ4EFgQUxhfQvKjqAkPyGwaZXSuQILnXnOQwDQYJKoZIhvcNAQELBQAD
ggIBADRh2Va1EodVTd2jNTFGu6QHcrxfYWLopfsLN7E8trP6KZ1/AvWkyaiTt3px
KGmPc+FSkNrVvjrlt3ZqVoAh313m6Tqe5T72omnHKgqwGEfcIHB9UqM+WXzBusnI
FUBhynLWcKzSt/Ac5IYp8M7vaGPQtSCKFWGafoaYtMnCdvvMujAWzKNhxnQT5Wvv
oxXqA/4Ti2Tk08HS6IT7SdEQTXlm66r99I0xHnAUrdzeZxNMgRVhvLfZkXdxGYFg
u/BYpbWcC/ePIlUnwEsBbTuZDdQdm2NnL9DuDcpmvJRPpq3t/O5jrFc/ZSXPsoaP
0Aj/uHYUbt7lJ+yreLVTubY/6CD50qi+YUbKh4yE8/nxoGibIh6BJpsQBJFxwAYf
3KDTuVan45gtf4Od34wrnDKOMpTwATwiKp9Dwi7DmDkHOHv8XgBCH/MyJnmDhPbl
8MFREsALHgQjDFSlTC9JxUrRtm5gDWv8a4uFJGS3iQ6rJUdbPM9+Sb3H6QrG2vd+
DhcI00iX0HGS8A85PjRqHH3Y8iKuu2n0M7SmSFXRDw4m6Oy2Cy2nhTXN/VnIn9HN
PlopNLk9hM6xZdRZkZFWdSHBd575euFgndOtBBj0fOtek49TSiIp+EgrPk2GrFt/
ywaZWWDYWGWVjUTR939+J399roD1B0y2PpxxVJkES/1Y+Zj0
-----END CERTIFICATE-----

# Issuer: CN=DigiCert Assured ID Root G2 O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert Assured ID Root G2 O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert Assured ID Root G2"
# Serial: 15385348160840213938643033620894905419
# MD5 Fingerprint: 92:38:b9:f8:63:24:82:65:2c:57:33:e6:fe:81:8f:9d
# SHA1 Fingerprint: a1:4b:48:d9:43:ee:0a:0e:40:90:4f:3c:e0:a4:c0:91:93:51:5d:3f
# SHA256 Fingerprint: 7d:05:eb:b6:82:33:9f:8c:94:51:ee:09:4e:eb:fe:fa:79:53:a1:14:ed:b2:f4:49:49:45:2f:ab:7d:2f:c1:85
-----BEGIN CERTIFICATE-----
MIIDljCCAn6gAwIBAgIQC5McOtY5Z+pnI7/Dr5r0SzANBgkqhkiG9w0BAQsFADBl
MQswCQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3
d3cuZGlnaWNlcnQuY29tMSQwIgYDVQQDExtEaWdpQ2VydCBBc3N1cmVkIElEIFJv
b3QgRzIwHhcNMTMwODAxMTIwMDAwWhcNMzgwMTE1MTIwMDAwWjBlMQswCQYDVQQG
EwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3d3cuZGlnaWNl
cnQuY29tMSQwIgYDVQQDExtEaWdpQ2VydCBBc3N1cmVkIElEIFJvb3QgRzIwggEi
MA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDZ5ygvUj82ckmIkzTz+GoeMVSA
n61UQbVH35ao1K+ALbkKz3X9iaV9JPrjIgwrvJUXCzO/GU1BBpAAvQxNEP4Htecc
biJVMWWXvdMX0h5i89vqbFCMP4QMls+3ywPgym2hFEwbid3tALBSfK+RbLE4E9Hp
EgjAALAcKxHad3A2m67OeYfcgnDmCXRwVWmvo2ifv922ebPynXApVfSr/5Vh88lA
bx3RvpO704gqu52/clpWcTs/1PPRCv4o76Pu2ZmvA9OPYLfykqGxvYmJHzDNw6Yu
YjOuFgJ3RFrngQo8p0Quebg/BLxcoIfhG69Rjs3sLPr4/m3wOnyqi+RnlTGNAgMB
AAGjQjBAMA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgGGMB0GA1UdDgQW
BBTOw0q5mVXyuNtgv6l+vVa1lzan1jANBgkqhkiG9w0BAQsFAAOCAQEAyqVVjOPI
QW5pJ6d1Ee88hjZv0p3GeDgdaZaikmkuOGybfQTUiaWxMTeKySHMq2zNixya1r9I
0jJmwYrA8y8678Dj1JGG0VDjA9tzd29KOVPt3ibHtX2vK0LRdWLjSisCx1BL4Gni
lmwORGYQRI+tBev4eaymG+g3NJ1TyWGqolKvSnAWhsI6yLETcDbYz+70CjTVW0z9
B5yiutkBclzzTcHdDrEcDcRjvq30FPuJ7KJBDkzMyFdA0G4Dqs0MjomZmWzwPDCv
ON9vvKO+KSAnq3T/EyJ43pdSVR6DtVQgA+6uwE9W3jfMw3+qBCe703e4YtsXfJwo
IhNzbM8m9Yop5w==
-----END CERTIFICATE-----

# Issuer: CN=DigiCert Assured ID Root G3 O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert Assured ID Root G3 O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert Assured ID Root G3"
# Serial: 15459312981008553731928384953135426796
# MD5 Fingerprint: 7c:7f:65:31:0c:81:df:8d:ba:3e:99:e2:5c:ad:6e:fb
# SHA1 Fingerprint: f5:17:a2:4f:9a:48:c6:c9:f8:a2:00:26:9f:dc:0f:48:2c:ab:30:89
# SHA256 Fingerprint: 7e:37:cb:8b:4c:47:09:0c:ab:36:55:1b:a6:f4:5d:b8:40:68:0f:ba:16:6a:95:2d:b1:00:71:7f:43:05:3f:c2
-----BEGIN CERTIFICATE-----
MIICRjCCAc2gAwIBAgIQC6Fa+h3foLVJRK/NJKBs7DAKBggqhkjOPQQDAzBlMQsw
CQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3d3cu
ZGlnaWNlcnQuY29tMSQwIgYDVQQDExtEaWdpQ2VydCBBc3N1cmVkIElEIFJvb3Qg
RzMwHhcNMTMwODAxMTIwMDAwWhcNMzgwMTE1MTIwMDAwWjBlMQswCQYDVQQGEwJV
UzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3d3cuZGlnaWNlcnQu
Y29tMSQwIgYDVQQDExtEaWdpQ2VydCBBc3N1cmVkIElEIFJvb3QgRzMwdjAQBgcq
hkjOPQIBBgUrgQQAIgNiAAQZ57ysRGXtzbg/WPuNsVepRC0FFfLvC/8QdJ+1YlJf
Zn4f5dwbRXkLzMZTCp2NXQLZqVneAlr2lSoOjThKiknGvMYDOAdfVdp+CW7if17Q
RSAPWXYQ1qAk8C3eNvJsKTmjQjBAMA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/
BAQDAgGGMB0GA1UdDgQWBBTL0L2p4ZgFUaFNN6KDec6NHSrkhDAKBggqhkjOPQQD
AwNnADBkAjAlpIFFAmsSS3V0T8gj43DydXLefInwz5FyYZ5eEJJZVrmDxxDnOOlY
JjZ91eQ0hjkCMHw2U/Aw5WJjOpnitqM7mzT6HtoQknFekROn3aRukswy1vUhZscv
6pZjamVFkpUBtA==
-----END CERTIFICATE-----

# Issuer: CN=DigiCert Global Root G2 O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert Global Root G2 O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert Global Root G2"
# Serial: 4293743540046975378534879503202253541
# MD5 Fingerprint: e4:a6:8a:c8:54:ac:52:42:46:0a:fd:72:48:1b:2a:44
# SHA1 Fingerprint: df:3c:24:f9:bf:d6:66:76:1b:26:80:73:fe:06:d1:cc:8d:4f:82:a4
# SHA256 Fingerprint: cb:3c:cb:b7:60:31:e5:e0:13:8f:8d:d3:9a:23:f9:de:47:ff:c3:5e:43:c1:14:4c:ea:27:d4:6a:5a:b1:cb:5f
-----BEGIN CERTIFICATE-----
MIIDjjCCAnagAwIBAgIQAzrx5qcRqaC7KGSxHQn65TANBgkqhkiG9w0BAQsFADBh
MQswCQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3
d3cuZGlnaWNlcnQuY29tMSAwHgYDVQQDExdEaWdpQ2VydCBHbG9iYWwgUm9vdCBH
MjAeFw0xMzA4MDExMjAwMDBaFw0zODAxMTUxMjAwMDBaMGExCzAJBgNVBAYTAlVT
MRUwEwYDVQQKEwxEaWdpQ2VydCBJbmMxGTAXBgNVBAsTEHd3dy5kaWdpY2VydC5j
b20xIDAeBgNVBAMTF0RpZ2lDZXJ0IEdsb2JhbCBSb290IEcyMIIBIjANBgkqhkiG
9w0BAQEFAAOCAQ8AMIIBCgKCAQEAuzfNNNx7a8myaJCtSnX/RrohCgiN9RlUyfuI
2/Ou8jqJkTx65qsGGmvPrC3oXgkkRLpimn7Wo6h+4FR1IAWsULecYxpsMNzaHxmx
1x7e/dfgy5SDN67sH0NO3Xss0r0upS/kqbitOtSZpLYl6ZtrAGCSYP9PIUkY92eQ
q2EGnI/yuum06ZIya7XzV+hdG82MHauVBJVJ8zUtluNJbd134/tJS7SsVQepj5Wz
tCO7TG1F8PapspUwtP1MVYwnSlcUfIKdzXOS0xZKBgyMUNGPHgm+F6HmIcr9g+UQ
vIOlCsRnKPZzFBQ9RnbDhxSJITRNrw9FDKZJobq7nMWxM4MphQIDAQABo0IwQDAP
BgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIBhjAdBgNVHQ4EFgQUTiJUIBiV
5uNu5g/6+rkS7QYXjzkwDQYJKoZIhvcNAQELBQADggEBAGBnKJRvDkhj6zHd6mcY
1Yl9PMWLSn/pvtsrF9+wX3N3KjITOYFnQoQj8kVnNeyIv/iPsGEMNKSuIEyExtv4
NeF22d+mQrvHRAiGfzZ0JFrabA0UWTW98kndth/Jsw1HKj2ZL7tcu7XUIOGZX1NG
Fdtom/DzMNU+MeKNhJ7jitralj41E6Vf8PlwUHBHQRFXGU7Aj64GxJUTFy8bJZ91
8rGOmaFvE7FBcf6IKshPECBV1/MUReXgRPTqh5Uykw7+U0b6LJ3/iyK5S9kJRaTe
pLiaWN0bfVKfjllDiIGknibVb63dDcY3fe0Dkhvld1927jyNxF1WW6LZZm6zNTfl
MrY=
-----END CERTIFICATE-----

# Issuer: CN=DigiCert Global Root G3 O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert Global Root G3 O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert Global Root G3"
# Serial: 7089244469030293291760083333884364146
# MD5 Fingerprint: f5:5d:a4:50:a5:fb:28:7e:1e:0f:0d:cc:96:57:56:ca
# SHA1 Fingerprint: 7e:04:de:89:6a:3e:66:6d:00:e6:87:d3:3f:fa:d9:3b:e8:3d:34:9e
# SHA256 Fingerprint: 31:ad:66:48:f8:10:41:38:c7:38:f3:9e:a4:32:01:33:39:3e:3a:18:cc:02:29:6e:f9:7c:2a:c9:ef:67:31:d0
-----BEGIN CERTIFICATE-----
MIICPzCCAcWgAwIBAgIQBVVWvPJepDU1w6QP1atFcjAKBggqhkjOPQQDAzBhMQsw
CQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3d3cu
ZGlnaWNlcnQuY29tMSAwHgYDVQQDExdEaWdpQ2VydCBHbG9iYWwgUm9vdCBHMzAe
Fw0xMzA4MDExMjAwMDBaFw0zODAxMTUxMjAwMDBaMGExCzAJBgNVBAYTAlVTMRUw
EwYDVQQKEwxEaWdpQ2VydCBJbmMxGTAXBgNVBAsTEHd3dy5kaWdpY2VydC5jb20x
IDAeBgNVBAMTF0RpZ2lDZXJ0IEdsb2JhbCBSb290IEczMHYwEAYHKoZIzj0CAQYF
K4EEACIDYgAE3afZu4q4C/sLfyHS8L6+c/MzXRq8NOrexpu80JX28MzQC7phW1FG
fp4tn+6OYwwX7Adw9c+ELkCDnOg/QW07rdOkFFk2eJ0DQ+4QE2xy3q6Ip6FrtUPO
Z9wj/wMco+I+o0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIBhjAd
BgNVHQ4EFgQUs9tIpPmhxdiuNkHMEWNpYim8S8YwCgYIKoZIzj0EAwMDaAAwZQIx
AK288mw/EkrRLTnDCgmXc/SINoyIJ7vmiI1Qhadj+Z4y3maTD/HMsQmP3Wyr+mt/
oAIwOWZbwmSNuJ5Q3KjVSaLtx9zRSX8XAbjIho9OjIgrqJqpisXRAL34VOKa5Vt8
sycX
-----END CERTIFICATE-----

# Issuer: CN=DigiCert Trusted Root G4 O=DigiCert Inc OU=www.digicert.com
# Subject: CN=DigiCert Trusted Root G4 O=DigiCert Inc OU=www.digicert.com
# Label: "DigiCert Trusted Root G4"
# Serial: 7451500558977370777930084869016614236
# MD5 Fingerprint: 78:f2:fc:aa:60:1f:2f:b4:eb:c9:37:ba:53:2e:75:49
# SHA1 Fingerprint: dd:fb:16:cd:49:31:c9:73:a2:03:7d:3f:c8:3a:4d:7d:77:5d:05:e4
# SHA256 Fingerprint: 55:2f:7b:dc:f1:a7:af:9e:6c:e6:72:01:7f:4f:12:ab:f7:72:40:c7:8e:76:1a:c2:03:d1:d9:d2:0a:c8:99:88
-----BEGIN CERTIFICATE-----
MIIFkDCCA3igAwIBAgIQBZsbV56OITLiOQe9p3d1XDANBgkqhkiG9w0BAQwFADBi
MQswCQYDVQQGEwJVUzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3
d3cuZGlnaWNlcnQuY29tMSEwHwYDVQQDExhEaWdpQ2VydCBUcnVzdGVkIFJvb3Qg
RzQwHhcNMTMwODAxMTIwMDAwWhcNMzgwMTE1MTIwMDAwWjBiMQswCQYDVQQGEwJV
UzEVMBMGA1UEChMMRGlnaUNlcnQgSW5jMRkwFwYDVQQLExB3d3cuZGlnaWNlcnQu
Y29tMSEwHwYDVQQDExhEaWdpQ2VydCBUcnVzdGVkIFJvb3QgRzQwggIiMA0GCSqG
SIb3DQEBAQUAA4ICDwAwggIKAoICAQC/5pBzaN675F1KPDAiMGkz7MKnJS7JIT3y
ithZwuEppz1Yq3aaza57G4QNxDAf8xukOBbrVsaXbR2rsnnyyhHS5F/WBTxSD1If
xp4VpX6+n6lXFllVcq9ok3DCsrp1mWpzMpTREEQQLt+C8weE5nQ7bXHiLQwb7iDV
ySAdYyktzuxeTsiT+CFhmzTrBcZe7FsavOvJz82sNEBfsXpm7nfISKhmV1efVFiO
DCu3T6cw2Vbuyntd463JT17lNecxy9qTXtyOj4DatpGYQJB5w3jHtrHEtWoYOAMQ
jdjUN6QuBX2I9YI+EJFwq1WCQTLX2wRzKm6RAXwhTNS8rhsDdV14Ztk6MUSaM0C/
CNdaSaTC5qmgZ92kJ7yhTzm1EVgX9yRcRo9k98FpiHaYdj1ZXUJ2h4mXaXpI8OCi
EhtmmnTK3kse5w5jrubU75KSOp493ADkRSWJtppEGSt+wJS00mFt6zPZxd9LBADM
fRyVw4/3IbKyEbe7f/LVjHAsQWCqsWMYRJUadmJ+9oCw++hkpjPRiQfhvbfmQ6QY
uKZ3AeEPlAwhHbJUKSWJbOUOUlFHdL4mrLZBdd56rF+NP8m800ERElvlEFDrMcXK
chYiCd98THU/Y+whX8QgUWtvsauGi0/C1kVfnSD8oR7FwI+isX4KJpn15GkvmB0t
9dmpsh3lGwIDAQABo0IwQDAPBgNVHRMBAf8EBTADAQH/MA4GA1UdDwEB/wQEAwIB
hjAdBgNVHQ4EFgQU7NfjgtJxXWRM3y5nP+e6mK4cD08wDQYJKoZIhvcNAQEMBQAD
ggIBALth2X2pbL4XxJEbw6GiAI3jZGgPVs93rnD5/ZpKmbnJeFwMDF/k5hQpVgs2
SV1EY+CtnJYYZhsjDT156W1r1lT40jzBQ0CuHVD1UvyQO7uYmWlrx8GnqGikJ9yd
+SeuMIW59mdNOj6PWTkiU0TryF0Dyu1Qen1iIQqAyHNm0aAFYF/opbSnr6j3bTWc
fFqK1qI4mfN4i/RN0iAL3gTujJtHgXINwBQy7zBZLq7gcfJW5GqXb5JQbZaNaHqa
sjYUegbyJLkJEVDXCLG4iXqEI2FCKeWjzaIgQdfRnGTZ6iahixTXTBmyUEFxPT9N
cCOGDErcgdLMMpSEDQgJlxxPwO5rIHQw0uA5NBCFIRUBCOhVMt5xSdkoF1BN5r5N
0XWs0Mr7QbhDparTwwVETyw2m+L64kW4I1NsBm9nVX9GtUw/bihaeSbSpKhil9Ie
4u1Ki7wb/UdKDd9nZn6yW0HQO+T0O/QEY+nvwlQAUaCKKsnOeMzV6ocEGLPOr0mI
r/OSmbaz5mEP0oUA51Aa5BuVnRmhuZyxm7EAHu/QD09CbMkKvO5D+jpxpchNJqU1
/YldvIViHTLSoCtU7ZpXwdv6EM8Zt4tKG48BtieVU+i2iW1bvGjUI+iLUaJW+fCm
gKDWHrO8Dw9TdSmq6hN35N6MgSGtBxBHEa2HPQfRdbzP82Z+
-----END CERTIFICATE-----

# Issuer: CN=Certification Authority of WoSign O=WoSign CA Limited
# Subject: CN=Certification Authority of WoSign O=WoSign CA Limited
# Label: "WoSign"
# Serial: 125491772294754854453622855443212256657
# MD5 Fingerprint: a1:f2:f9:b5:d2:c8:7a:74:b8:f3:05:f1:d7:e1:84:8d
# SHA1 Fingerprint: b9:42:94:bf:91:ea:8f:b6:4b:e6:10:97:c7:fb:00:13:59:b6:76:cb
# SHA256 Fingerprint: 4b:22:d5:a6:ae:c9:9f:3c:db:79:aa:5e:c0:68:38:47:9c:d5:ec:ba:71:64:f7:f2:2d:c1:d6:5f:63:d8:57:08
-----BEGIN CERTIFICATE-----
MIIFdjCCA16gAwIBAgIQXmjWEXGUY1BWAGjzPsnFkTANBgkqhkiG9w0BAQUFADBV
MQswCQYDVQQGEwJDTjEaMBgGA1UEChMRV29TaWduIENBIExpbWl0ZWQxKjAoBgNV
BAMTIUNlcnRpZmljYXRpb24gQXV0aG9yaXR5IG9mIFdvU2lnbjAeFw0wOTA4MDgw
MTAwMDFaFw0zOTA4MDgwMTAwMDFaMFUxCzAJBgNVBAYTAkNOMRowGAYDVQQKExFX
b1NpZ24gQ0EgTGltaXRlZDEqMCgGA1UEAxMhQ2VydGlmaWNhdGlvbiBBdXRob3Jp
dHkgb2YgV29TaWduMIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAvcqN
rLiRFVaXe2tcesLea9mhsMMQI/qnobLMMfo+2aYpbxY94Gv4uEBf2zmoAHqLoE1U
fcIiePyOCbiohdfMlZdLdNiefvAA5A6JrkkoRBoQmTIPJYhTpA2zDxIIFgsDcScc
f+Hb0v1naMQFXQoOXXDX2JegvFNBmpGN9J42Znp+VsGQX+axaCA2pIwkLCxHC1l2
ZjC1vt7tj/id07sBMOby8w7gLJKA84X5KIq0VC6a7fd2/BVoFutKbOsuEo/Uz/4M
x1wdC34FMr5esAkqQtXJTpCzWQ27en7N1QhatH/YHGkR+ScPewavVIMYe+HdVHpR
aG53/Ma/UkpmRqGyZxq7o093oL5d//xWC0Nyd5DKnvnyOfUNqfTq1+ezEC8wQjch
zDBwyYaYD8xYTYO7feUapTeNtqwylwA6Y3EkHp43xP901DfA4v6IRmAR3Qg/UDar
uHqklWJqbrDKaiFaafPz+x1wOZXzp26mgYmhiMU7ccqjUu6Du/2gd/Tkb+dC221K
mYo0SLwX3OSACCK28jHAPwQ+658geda4BmRkAjHXqc1S+4RFaQkAKtxVi8QGRkvA
Sh0JWzko/amrzgD5LkhLJuYwTKVYyrREgk/nkR4zw7CT/xH8gdLKH3Ep3XZPkiWv
HYG3Dy+MwwbMLyejSuQOmbp8HkUff6oZRZb9/D0CAwEAAaNCMEAwDgYDVR0PAQH/
BAQDAgEGMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFOFmzw7R8bNLtwYgFP6H
EtX2/vs+MA0GCSqGSIb3DQEBBQUAA4ICAQCoy3JAsnbBfnv8rWTjMnvMPLZdRtP1
LOJwXcgu2AZ9mNELIaCJWSQBnfmvCX0KI4I01fx8cpm5o9dU9OpScA7F9dY74ToJ
MuYhOZO9sxXqT2r09Ys/L3yNWC7F4TmgPsc9SnOeQHrAK2GpZ8nzJLmzbVUsWh2e
JXLOC62qx1ViC777Y7NhRCOjy+EaDveaBk3e1CNOIZZbOVtXHS9dCF4Jef98l7VN
g64N1uajeeAz0JmWAjCnPv/So0M/BVoG6kQC2nz4SNAzqfkHx5Xh9T71XXG68pWp
dIhhWeO/yloTunK0jF02h+mmxTwTv97QRCbut+wucPrXnbes5cVAWubXbHssw1ab
R80LzvobtCHXt2a49CUwi1wNuepnsvRtrtWhnk/Yn+knArAdBtaP4/tIEp9/EaEQ
PkxROpaw0RPxx9gmrjrKkcRpnd8BKWRRb2jaFOwIQZeQjdCygPLPwj2/kWjFgGce
xGATVdVhmVd8upUPYUk6ynW8yQqTP2cOEvIo4jEbwFcW3wh8GcF+Dx+FHgo2fFt+
J7x6v+Db9NpSvd4MVHAxkUOVyLzwPt0JfjBkUO1/AaQzZ01oT74V77D2AhGiGxMl
OtzCWfHjXEa7ZywCRuoeSKbmW9m1vFGikpbbqsY3Iqb+zCB0oy2pLmvLwIIRIbWT
ee5Ehr7XHuQe+w==
-----END CERTIFICATE-----

# Issuer: CN=CA 沃通根证书 O=WoSign CA Limited
# Subject: CN=CA 沃通根证书 O=WoSign CA Limited
# Label: "WoSign China"
# Serial: 106921963437422998931660691310149453965
# MD5 Fingerprint: 78:83:5b:52:16:76:c4:24:3b:83:78:e8:ac:da:9a:93
# SHA1 Fingerprint: 16:32:47:8d:89:f9:21:3a:92:00:85:63:f5:a4:a7:d3:12:40:8a:d6
# SHA256 Fingerprint: d6:f0:34:bd:94:aa:23:3f:02:97:ec:a4:24:5b:28:39:73:e4:47:aa:59:0f:31:0c:77:f4:8f:df:83:11:22:54
-----BEGIN CERTIFICATE-----
MIIFWDCCA0CgAwIBAgIQUHBrzdgT/BtOOzNy0hFIjTANBgkqhkiG9w0BAQsFADBG
MQswCQYDVQQGEwJDTjEaMBgGA1UEChMRV29TaWduIENBIExpbWl0ZWQxGzAZBgNV
BAMMEkNBIOayg+mAmuagueivgeS5pjAeFw0wOTA4MDgwMTAwMDFaFw0zOTA4MDgw
MTAwMDFaMEYxCzAJBgNVBAYTAkNOMRowGAYDVQQKExFXb1NpZ24gQ0EgTGltaXRl
ZDEbMBkGA1UEAwwSQ0Eg5rKD6YCa5qC56K+B5LmmMIICIjANBgkqhkiG9w0BAQEF
AAOCAg8AMIICCgKCAgEA0EkhHiX8h8EqwqzbdoYGTufQdDTc7WU1/FDWiD+k8H/r
D195L4mx/bxjWDeTmzj4t1up+thxx7S8gJeNbEvxUNUqKaqoGXqW5pWOdO2XCld1
9AXbbQs5uQF/qvbW2mzmBeCkTVL829B0txGMe41P/4eDrv8FAxNXUDf+jJZSEExf
v5RxadmWPgxDT74wwJ85dE8GRV2j1lY5aAfMh09Qd5Nx2UQIsYo06Yms25tO4dnk
UkWMLhQfkWsZHWgpLFbE4h4TV2TwYeO5Ed+w4VegG63XX9Gv2ystP9Bojg/qnw+L
NVgbExz03jWhCl3W6t8Sb8D7aQdGctyB9gQjF+BNdeFyb7Ao65vh4YOhn0pdr8yb
+gIgthhid5E7o9Vlrdx8kHccREGkSovrlXLp9glk3Kgtn3R46MGiCWOc76DbT52V
qyBPt7D3h1ymoOQ3OMdc4zUPLK2jgKLsLl3Az+2LBcLmc272idX10kaO6m1jGx6K
yX2m+Jzr5dVjhU1zZmkR/sgO9MHHZklTfuQZa/HpelmjbX7FF+Ynxu8b22/8DU0G
AbQOXDBGVWCvOGU6yke6rCzMRh+yRpY/8+0mBe53oWprfi1tWFxK1I5nuPHa1UaK
J/kR8slC/k7e3x9cxKSGhxYzoacXGKUN5AXlK8IrC6KVkLn9YDxOiT7nnO4fuwEC
AwEAAaNCMEAwDgYDVR0PAQH/BAQDAgEGMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0O
BBYEFOBNv9ybQV0T6GTwp+kVpOGBwboxMA0GCSqGSIb3DQEBCwUAA4ICAQBqinA4
WbbaixjIvirTthnVZil6Xc1bL3McJk6jfW+rtylNpumlEYOnOXOvEESS5iVdT2H6
yAa+Tkvv/vMx/sZ8cApBWNromUuWyXi8mHwCKe0JgOYKOoICKuLJL8hWGSbueBwj
/feTZU7n85iYr83d2Z5AiDEoOqsuC7CsDCT6eiaY8xJhEPRdF/d+4niXVOKM6Cm6
jBAyvd0zaziGfjk9DgNyp115j0WKWa5bIW4xRtVZjc8VX90xJc/bYNaBRHIpAlf2
ltTW/+op2znFuCyKGo3Oy+dCMYYFaA6eFN0AkLppRQjbbpCBhqcqBT/mhDn4t/lX
X0ykeVoQDF7Va/81XwVRHmyjdanPUIPTfPRm94KNPQx96N97qA4bLJyuQHCH2u2n
FoJavjVsIE4iYdm8UXrNemHcSxH5/mc0zy4EZmFcV5cjjPOGG0jfKq+nwf/Yjj4D
u9gqsPoUJbJRa4ZDhS4HIxaAjUz7tGM7zMN07RujHv41D198HRaG9Q7DlfEvr10l
O1Hm13ZBONFLAzkopR6RctR9q5czxNM+4Gm2KHmgCY0c0f9BckgG/Jou5yD5m6Le
ie2uPAmvylezkolwQOQvT8Jwg0DXJCxr5wkf09XHwQj02w47HAcLQxGEIYbpgNR1
2KvxAmLBsX5VYc8T1yaw15zLKYs4SgsOkI26oQ==
-----END CERTIFICATE-----

`

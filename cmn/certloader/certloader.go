// Package certloader loads and reloads X.509 certs.
/*
 * Copyright (c) 2024, NVIDIA CORPORATION. All rights reserved.
 */
package certloader

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/hk"
)

const name = "tls-cert-loader"

const dfltTimeInvalid = time.Hour

const fmtErrExpired = "%s: %s expired (valid until %v)"

type (
	xcert struct {
		tls.Certificate
		parent    *certLoader
		modTime   time.Time
		notBefore time.Time
		notAfter  time.Time
		size      int64
	}
	certLoader struct {
		tstats   cos.StatsUpdater
		certFile string
		keyFile  string
		xcert    atomic.Pointer[xcert]
	}

	// tls.Config.GetCertificate
	GetCertCB func(_ *tls.ClientHelloInfo) (*tls.Certificate, error)

	// tls.Config.GetClientCertificate
	GetClientCertCB func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error)

	errExpired struct {
		msg string
	}
)

var (
	gcl *certLoader
)

// (htrun only)
func Init(certFile, keyFile string, tstats cos.StatsUpdater) (err error) {
	if certFile == "" && keyFile == "" {
		return nil
	}

	debug.Assert(gcl == nil)
	gcl = &certLoader{certFile: certFile, keyFile: keyFile, tstats: tstats}
	if err = Load(); err != nil {
		nlog.Errorln("FATAL:", err)
		return err
	}

	hk.Reg(name, gcl.hk, gcl.hktime())
	return nil
}

// via (Init, API call)
func Load() (err error) {
	if err = gcl.do(false /*compare*/); err == nil {
		return nil
	}
	if isExpired(err) {
		gcl.tstats.SetFlag(cos.NodeAlerts, cos.CertificateExpired)
	} else {
		gcl.tstats.SetFlag(cos.NodeAlerts, cos.CertificateInvalid)
	}
	return err
}

func (cl *certLoader) hk() time.Duration {
	if err := cl.do(true /*compare*/); err != nil {
		nlog.Errorln(err)
	}
	return cl.hktime()
}

func (cl *certLoader) hktime() (d time.Duration) {
	flags := cos.NodeStateFlags(cl.tstats.Get(cos.NodeAlerts))
	if flags.IsSet(cos.CertificateExpired) || flags.IsSet(cos.CertificateInvalid) {
		return dfltTimeInvalid
	}

	// (still) valid
	const warn = "X.509 will soon expire - remains:"
	rem := time.Until(cl.xcert.Load().notAfter)
	switch {
	case rem > 24*time.Hour:
		d = 6 * time.Hour
	case rem > 6*time.Hour:
		d = time.Hour
	case rem > time.Hour:
		d = 10 * time.Minute
	case rem > 10*time.Minute:
		nlog.Warningln(cl.certFile, warn, rem)
		d = time.Minute
	case rem > 0:
		nlog.Errorln(cl.certFile, warn, rem)
		d = time.Minute
	default: // expired
		cl.tstats.SetFlag(cos.NodeAlerts, cos.CertificateExpired)
		d = dfltTimeInvalid
	}
	return d
}

func (cl *certLoader) errorf() error {
	flags := cos.NodeStateFlags(cl.tstats.Get(cos.NodeAlerts))
	switch {
	case flags.IsSet(cos.CertificateInvalid):
		return fmt.Errorf("%s: (%s, %s) is invalid", name, cl.certFile, cl.keyFile)
	case flags.IsSet(cos.CertificateExpired):
		xcert := cl.xcert.Load()
		msg := fmt.Sprintf(fmtErrExpired, name, cl.certFile, xcert.notAfter)
		return &errExpired{msg}
	default:
		return nil
	}
}

func (cl *certLoader) _get() *tls.Certificate { return &cl.xcert.Load().Certificate }

func (cl *certLoader) _hello(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cl._get(), nil }

func GetCert() (GetCertCB, error) {
	debug.Assert(gcl != nil, name, " not initialized")
	if err := gcl.errorf(); err != nil {
		return nil, err
	}
	return gcl._hello, nil
}

func (cl *certLoader) _info(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return cl._get(), nil
}

func GetClientCert() (GetClientCertCB, error) {
	debug.Assert(gcl != nil, name, " not initialized")
	if err := gcl.errorf(); err != nil {
		return nil, err
	}
	return gcl._info, nil
}

func (cl *certLoader) do(compare bool) (err error) {
	var (
		finfo os.FileInfo
		xcert = xcert{parent: cl}
	)
	// 1. fstat
	finfo, err = os.Stat(cl.certFile)
	if err != nil {
		return fmt.Errorf("%s: failed to fstat %q, err: %w", name, cl.certFile, err)
	}

	// 2. updated?
	if compare {
		xcert := cl.xcert.Load()
		debug.Assert(xcert != nil, "expecting X.509 loaded at startup: ", cl.certFile, ", ", cl.keyFile)
		if finfo.ModTime() == xcert.modTime && finfo.Size() == xcert.size {
			return nil
		}
	}

	// 3. read and parse
	xcert.Certificate, err = tls.LoadX509KeyPair(cl.certFile, cl.keyFile)
	if err != nil {
		return fmt.Errorf("%s: failed to load (%s, %s), err: %w", name, cl.certFile, cl.keyFile, err)
	}
	if err = xcert.ini(finfo); err != nil {
		return err
	}

	// 4. ok
	cl.tstats.ClrFlag(cos.NodeAlerts, cos.CertificateExpired|cos.CertificateInvalid)
	cl.xcert.Store(&xcert)

	nlog.Infoln(xcert.String())
	return nil
}

///////////
// xcert //
///////////

func (x *xcert) String() string {
	var sb strings.Builder
	sb.WriteString(x.parent.certFile)

	sb.WriteByte('[')
	sb.WriteString(cos.FormatTime(x.notBefore, ""))
	sb.WriteByte(',')
	sb.WriteString(cos.FormatTime(x.notAfter, ""))
	sb.WriteByte(']')

	return sb.String()
}

// NOTE: second time parsing certificate (first time in tls.LoadX509KeyPair above)
// to find out valid time bounds
func (x *xcert) ini(finfo os.FileInfo) (err error) {
	if x.Certificate.Leaf == nil {
		x.Certificate.Leaf, err = x509.ParseCertificate(x.Certificate.Certificate[0])
		if err != nil {
			return fmt.Errorf("%s: failed to parse %q, err: %w", name, x.parent.certFile, err)
		}
	}
	{
		x.modTime = finfo.ModTime()
		x.size = finfo.Size()
		x.notBefore = x.Certificate.Leaf.NotBefore
		x.notAfter = x.Certificate.Leaf.NotAfter
	}
	now := time.Now()
	if now.After(x.notAfter) {
		msg := fmt.Sprintf(fmtErrExpired, name, x.parent.certFile, x.notAfter)
		err = &errExpired{msg}
	} else if now.Before(x.notBefore) {
		nlog.Warningln(x.parent.certFile, "is not valid _yet_: [", x.notBefore, x.notAfter, "]")
	}
	return err
}

//
// other
//

func (e *errExpired) Error() string { return e.msg }

func isExpired(err error) bool {
	_, ok := err.(*errExpired)
	return ok
}
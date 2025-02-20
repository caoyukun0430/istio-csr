/*
Copyright 2021 The cert-manager Authors.

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

package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	securityapi "istio.io/api/security/v1alpha1"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/security"
	pkiutil "istio.io/istio/security/pkg/pki/util"

	"github.com/cert-manager/istio-csr/pkg/server/internal/extensions"
)

// authRequest will authenticate the request and authorize the CSR is valid for
// the identity
func (s *Server) authRequest(ctx context.Context, icr *securityapi.IstioCertificateRequest) (string, bool) {
	var caller *security.Caller
	var errs []error
	found := false
	for _, authenticator := range s.authenticators {
		var err error
		caller, err = authenticator.Authenticate(security.AuthContext{GrpcContext: ctx})
		if err == nil {
			found = true
			break
		}
		errs = append(errs, err)
	}
	if !found {
		// TODO: pass in logger with request context
		s.log.Error(errors.Join(errs...), "failed to authenticate request")
		return "", false
	}

	// request authentication has no identities, so error
	if len(caller.Identities) == 0 {
		s.log.Error(errors.New("request sent with no identity"), "")
		return "", false
	}

	var identities string

	crMetadata := icr.GetMetadata().GetFields()
	impersonatedIdentity := crMetadata[security.ImpersonatedIdentity].GetStringValue()
	if impersonatedIdentity != "" {
		log.Debugf("impersonated identity: %s", impersonatedIdentity)
		if s.nodeAuthorizer == nil {
			log.Warnf("impersonation not allowed, as node authorizer (CA_TRUSTED_NODE_ACCOUNTS) is not configured")
			return "", false
		}
		if err := s.nodeAuthorizer.authenticateImpersonation(caller.KubernetesInfo, impersonatedIdentity); err != nil {
			log.Error(fmt.Errorf("failed to validate impersonated identity %v: %v", impersonatedIdentity, err))
			return identities, false
		}
		identities = impersonatedIdentity
	} else {
		identities = strings.Join(caller.Identities, ",")
	}

	// return concatenated list of verified ids
	log := s.log.WithValues("identities", identities)

	csr, err := pkiutil.ParsePemEncodedCSR([]byte(icr.GetCsr()))
	if err != nil {
		log.Error(err, "failed to decode CSR")
		return identities, false
	}

	if err := csr.CheckSignature(); err != nil {
		log.Error(err, "CSR failed signature check")
		return identities, false
	}

	// if the csr contains any other options set, error
	if len(csr.IPAddresses) > 0 || len(csr.EmailAddresses) > 0 {
		log.Error(errors.New("forbidden extensions"), "",
			"ips", csr.IPAddresses,
			"emails", csr.EmailAddresses)

		return identities, false
	}

	// ensure csr extensions are valid
	if err := extensions.ValidateCSRExtentions(csr); err != nil {
		log.Error(err, "forbidden extensions")
		return identities, false
	}

	if impersonatedIdentity == "" {
		if !identitiesMatch(caller.Identities, csr.URIs) {
			log.Error(fmt.Errorf("%v != %v", caller.Identities, csr.URIs), "failed to match URIs with identities")
			return identities, false
		}
	} else if !identitiesMatch([]string{impersonatedIdentity}, csr.URIs) {
		log.Error(fmt.Errorf("%v != %v", impersonatedIdentity, csr.URIs), "failed to match URIs with impersonated identities")
		return identities, false
	}

	// return positive authn of given csr
	return identities, true
}

// identitiesMatch will ensure that two list of identities given from the
// request context, and those parsed from the CSR, match
func identitiesMatch(a []string, b []*url.URL) bool {
	if len(a) != len(b) {
		return false
	}

	aa := make([]string, len(a))
	bb := make([]*url.URL, len(b))

	copy(aa, a)
	copy(bb, b)

	sort.Strings(aa)
	sort.SliceStable(bb, func(i, j int) bool {
		return bb[i].String() < bb[j].String()
	})

	for i, v := range aa {
		if bb[i].String() != v {
			return false
		}
	}

	return true
}

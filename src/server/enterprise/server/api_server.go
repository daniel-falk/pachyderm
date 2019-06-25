package server

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/types"
	logrus "github.com/sirupsen/logrus"
	"golang.org/x/net/context"

	ec "github.com/pachyderm/pachyderm/src/client/enterprise"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/log"
	"github.com/pachyderm/pachyderm/src/server/pkg/serviceenv"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
)

const (
	enterprisePrefix = "/enterprise"

	publicKey = `-----BEGIN PUBLIC KEY-----
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAtJnDuD05fJZVsWDvN/un
m5xbG7jcmxUsSOQZfvMaafZjV6iG/z6Wst2uhcMGAMrLHBxFiRYiVVM3kbUhbfbw
3nVzALDLh4l/QzovCcF12FzVY8fB5Q6VQFfnup1aKimyJX7/au0ihvv//olQ1xrL
XRaG7h/hnCbmjLhsaGA6nqB4gtRI+HI3tBvQBicaN0P5pcfJlT49BSgJq6pnbZPY
SmXeL5m/o1sWZzjzlkmXuxxptG8WTDU3cYF2wmGNMDV/e7u7TuvnFLEz+xf8MUcq
LrDaDj1OuQVwftfz+jqZunQifx4pq6Sxk3ecQll2OhHE1LHrDdE+KSYumUVr0h5i
OVro2tqn4CUmwWrDb4O3TxowrNHylXWAWsLukXQCxguYPRRdIlpu8QPYvsdjU0xT
F7sRv8juuBMSOwRnEZE0M0E/XeLiJo9ROzVxHbRga2AHgDtt0rVHrUrlKmJFJyU2
DACvluEWcjXKXRJJkeieSQopITTQtBSYVu0fr1HG1pLOs1ZakPRPUi/xnSnDb2zK
XinORcb47IsWIHXtwHcwY1C7kV0IK3DxJrJZsSib171vAwi6q/HSOSkWxCURsOtK
x90hW9XbejJCpAiOYfPEOq0lT8fy1Ve0qBen1y4mcxtnXANrgQyYCCBftoc7Ctkk
m5MuBYYSa4PH/uIZktTYOkMCAwEAAQ==
-----END PUBLIC KEY-----
`

	// enterpriseTokenKey is the constant key we use that maps to an Enterprise
	// token that a user has given us. This is what we check to know if a
	// Pachyderm cluster supports enterprise features
	enterpriseTokenKey = "token"
)

type apiServer struct {
	pachLogger log.Logger
	env        *serviceenv.ServiceEnv

	// enterpriseState is a cached timestamp, indicating when the current
	// Pachyderm Enterprise token will expire (or 0 if there is no Pachyderm
	// Enterprise token
	enterpriseExpiration atomic.Value

	// A default record that expired long, long ago (in this galaxy).
	defaultEnterpriseRecord *ec.EnterpriseRecord

	// enterpriseToken is a collection containing at most one Pachyderm enterprise
	// token
	enterpriseToken col.Collection
}

func (a *apiServer) LogReq(request interface{}) {
	a.pachLogger.Log(request, nil, nil, 0)
}

// NewEnterpriseServer returns an implementation of ec.APIServer.
func NewEnterpriseServer(env *serviceenv.ServiceEnv, etcdPrefix string) (ec.APIServer, error) {
	defaultExpires, err := types.TimestampProto(time.Time{})
	if err != nil {
		return nil, err
	}
	s := &apiServer{
		pachLogger: log.NewLogger("enterprise.API"),
		env:        env,
		enterpriseToken: col.NewCollection(
			env.GetEtcdClient(),
			etcdPrefix, // only one collection--no extra prefix needed
			nil,
			&ec.EnterpriseRecord{},
			nil,
			nil,
		),
		defaultEnterpriseRecord: &ec.EnterpriseRecord{Expires: defaultExpires},
	}
	s.enterpriseExpiration.Store(s.defaultEnterpriseRecord)
	go s.watchEnterpriseToken(etcdPrefix)
	return s, nil
}

func (a *apiServer) watchEnterpriseToken(etcdPrefix string) {
	backoff.RetryNotify(func() error {
		// Watch for incoming enterprise tokens
		watcher, err := a.enterpriseToken.ReadOnly(context.Background()).Watch()
		if err != nil {
			return err
		}
		defer watcher.Close()
		for {
			ev, ok := <-watcher.Watch()
			if !ok {
				return errors.New("admin watch closed unexpectedly")
			}

			// Parse event data and potentially update adminCache
			switch ev.Type {
			case watch.EventPut:
				var key string
				record := &ec.EnterpriseRecord{}
				if err := ev.Unmarshal(&key, record); err != nil {
					return err
				}
				a.enterpriseExpiration.Store(record)
			case watch.EventDelete:
				// This should only occur if the etcd value is deleted via the etcd API,
				// but that does occur during testing
				a.enterpriseExpiration.Store(a.defaultEnterpriseRecord)
			case watch.EventError:
				return ev.Err
			}
		}
	}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
		logrus.Printf("error from activation check: %v; retrying in %v", err, d)
		return nil
	})
}

type activationCode struct {
	Token     string
	Signature string
}

// token is used to parse a JSON object generated by Pachyderm Inc's enterprise
// token tool. Note that if you want to change this struct, you'll have to
// change the enterprise token tool and potentially generate new tokens for all
// of Pachyderm's customers (if you're changing or removing a field).
type token struct {
	Expiry string
}

// validateActivationCode checks the validity of an activation code
func validateActivationCode(code string) (expiration time.Time, err error) {
	// Parse the public key.  If these steps fail, something is seriously
	// wrong and we should crash the service by panicking.
	block, _ := pem.Decode([]byte(publicKey))
	if block == nil {
		return time.Time{}, fmt.Errorf("failed to pem decode public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse DER encoded public key: %s", err.Error())
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return time.Time{}, fmt.Errorf("public key isn't an RSA key")
	}

	// Decode the base64-encoded activation code
	decodedActivationCode, err := base64.StdEncoding.DecodeString(code)
	if err != nil {
		return time.Time{}, fmt.Errorf("activation code is not base64 encoded")
	}
	activationCode := &activationCode{}
	if err := json.Unmarshal(decodedActivationCode, &activationCode); err != nil {
		return time.Time{}, fmt.Errorf("activation code is not valid JSON")
	}

	// Decode the signature
	decodedSignature, err := base64.StdEncoding.DecodeString(activationCode.Signature)
	if err != nil {
		return time.Time{}, fmt.Errorf("signature is not base64 encoded")
	}

	// Compute the sha256 checksum of the token
	hashedToken := sha256.Sum256([]byte(activationCode.Token))

	// Verify that the signature is valid
	if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hashedToken[:], decodedSignature); err != nil {
		return time.Time{}, fmt.Errorf("invalid signature in activation code")
	}

	// Unmarshal the token
	token := token{}
	if err := json.Unmarshal([]byte(activationCode.Token), &token); err != nil {
		return time.Time{}, fmt.Errorf("token is not valid JSON")
	}

	// Parse the expiration. Note that this string is generated by Date.toJSON()
	// running in node, so Go's definition of RFC 3339 timestamps (which is
	// incomplete) must be compatible with the strings that node generates. So far
	// it seems to work.
	expiration, err = time.Parse(time.RFC3339, token.Expiry)
	if err != nil {
		return time.Time{}, fmt.Errorf("expiration is not valid ISO 8601 string")
	}
	// Check that the activation code has not expired
	if time.Now().After(expiration) {
		return time.Time{}, fmt.Errorf("the activation code has expired")
	}
	return expiration, nil
}

// Activate implements the Activate RPC
func (a *apiServer) Activate(ctx context.Context, req *ec.ActivateRequest) (resp *ec.ActivateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	// Validate the activation code
	expiration, err := validateActivationCode(req.ActivationCode)
	if err != nil {
		return nil, fmt.Errorf("error validating activation code: %s", err.Error())
	}
	// Allow request to override expiration in the activation code, for testing
	if req.Expires != nil {
		customExpiration, err := types.TimestampFromProto(req.Expires)
		if err == nil && expiration.After(customExpiration) {
			expiration = customExpiration
		}
	}
	expirationProto, err := types.TimestampProto(expiration)
	if err != nil {
		return nil, fmt.Errorf("could not convert expiration time \"%s\" to proto: %s", expiration.String(), err.Error())
	}
	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		e := a.enterpriseToken.ReadWrite(stm)
		// blind write
		return e.Put(enterpriseTokenKey, &ec.EnterpriseRecord{
			ActivationCode: req.ActivationCode,
			Expires:        expirationProto,
		})
	}); err != nil {
		return nil, err
	}

	// Wait until watcher observes the write
	if err := backoff.Retry(func() error {
		record, ok := a.enterpriseExpiration.Load().(*ec.EnterpriseRecord)
		if !ok {
			return fmt.Errorf("could not retrieve enterprise expiration time")
		}
		expiration, err := types.TimestampFromProto(record.Expires)
		if err != nil {
			return fmt.Errorf("could not parse expiration timestamp: %s", err.Error())
		}
		if expiration.IsZero() {
			return fmt.Errorf("enterprise not activated")
		}
		return nil
	}, backoff.RetryEvery(time.Second)); err != nil {
		return nil, err
	}
	time.Sleep(time.Second) // give other pachd nodes time to observe the write

	return &ec.ActivateResponse{
		Info: &ec.TokenInfo{
			Expires: expirationProto,
		},
	}, nil
}

// GetState returns the current state of the cluster's Pachyderm Enterprise key (ACTIVE, EXPIRED, or NONE)
func (a *apiServer) GetState(ctx context.Context, req *ec.GetStateRequest) (resp *ec.GetStateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	record, ok := a.enterpriseExpiration.Load().(*ec.EnterpriseRecord)
	if !ok {
		return nil, fmt.Errorf("could not retrieve enterprise expiration time")
	}
	expiration, err := types.TimestampFromProto(record.Expires)
	if err != nil {
		return nil, fmt.Errorf("could not parse expiration timestamp: %s", err.Error())
	}
	if expiration.IsZero() {
		return &ec.GetStateResponse{State: ec.State_NONE}, nil
	}
	resp = &ec.GetStateResponse{
		Info: &ec.TokenInfo{
			Expires: record.Expires,
		},
		ActivationCode: record.ActivationCode,
	}
	if time.Now().After(expiration) {
		resp.State = ec.State_EXPIRED
	} else {
		resp.State = ec.State_ACTIVE
	}
	return resp, nil
}

// Deactivate deletes the current cluster's enterprise token, and puts the
// cluster in the "NONE" enterprise state. It also deletes all data in the
// cluster, to avoid invalid cluster states. This call only makes sense for
// testing
func (a *apiServer) Deactivate(ctx context.Context, req *ec.DeactivateRequest) (resp *ec.DeactivateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	pachClient := a.env.GetPachClient(ctx)
	if err := pachClient.DeleteAll(); err != nil {
		return nil, fmt.Errorf("could not delete all pachyderm data: %v", err)
	}

	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		err := a.enterpriseToken.ReadWrite(stm).Delete(enterpriseTokenKey)
		if err != nil && !col.IsErrNotFound(err) {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Wait until watcher observes the write
	if err := backoff.Retry(func() error {
		record, ok := a.enterpriseExpiration.Load().(*ec.EnterpriseRecord)
		if !ok {
			return fmt.Errorf("could not retrieve enterprise expiration time")
		}
		expiration, err := types.TimestampFromProto(record.Expires)
		if err != nil {
			return fmt.Errorf("could not parse expiration timestamp: %s", err.Error())
		}
		if !expiration.IsZero() {
			return fmt.Errorf("enterprise still activated")
		}
		return nil
	}, backoff.RetryEvery(time.Second)); err != nil {
		return nil, err
	}
	time.Sleep(time.Second) // give other pachd nodes time to observe the write

	return &ec.DeactivateResponse{}, nil
}

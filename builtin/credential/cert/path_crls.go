package cert

import (
	"crypto/x509"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/fatih/structs"
	"github.com/hashicorp/vault/helper/certutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

func pathCRLs(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "crls/" + framework.GenericNameRegex("name"),
		Fields: map[string]*framework.FieldSchema{
			"name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "The name of the certificate",
			},

			"crl": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `The public certificate that should be trusted.
May be DER or PEM encoded. Note: the expiration time
is ignored; if the CRL is no longer valid, delete it
using the same name as specified here.`,
			},

			"serial": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `If specified, for a read, information for this
serial will be returned rather than the named CRL.
This can be a hex-formatted string separated
by : or -, or an integer string; this will be
assumed to be base 10 unless prefixed by "0x"
for base 16 or "0" for base 8.`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.DeleteOperation: b.pathCRLDelete,
			logical.ReadOperation:   b.pathCRLRead,
			logical.WriteOperation:  b.pathCRLWrite,
		},

		HelpSynopsis:    pathCRLsHelpSyn,
		HelpDescription: pathCRLsHelpDesc,
	}
}

var (
	crls           = map[string]CRLInfo{}
	crlUpdateMutex = &sync.RWMutex{}
)

func populateCRLs(storage logical.Storage) error {
	crlUpdateMutex.Lock()
	defer crlUpdateMutex.Unlock()

	keys, err := storage.List("crls/")
	if err != nil {
		return fmt.Errorf("error listing CRLs: %v", err)
	}
	if keys == nil || len(keys) == 0 {
		return nil
	}

	for _, key := range keys {
		entry, err := storage.Get("crls/" + key)
		if err != nil {
			return fmt.Errorf("error loading CRL %s: %v", key, err)
		}
		if entry == nil {
			continue
		}
		var crlInfo CRLInfo
		err = entry.DecodeJSON(&crlInfo)
		if err != nil {
			return fmt.Errorf("error decoding CRL %s: %v", key, err)
		}
		crls[key] = crlInfo
	}

	return nil
}

func findSerialInCRLs(serial *big.Int) map[string]RevokedSerialInfo {
	crlUpdateMutex.RLock()
	defer crlUpdateMutex.RUnlock()
	ret := map[string]RevokedSerialInfo{}
	for key, crl := range crls {
		if crl.Serials == nil {
			continue
		}
		if info, ok := crl.Serials[serial.String()]; ok {
			ret[key] = info
		}
	}
	return ret
}

func parseSerialString(input string) (*big.Int, error) {
	ret := &big.Int{}

	switch {
	case strings.Count(input, ":") > 0:
		serialBytes := certutil.ParseHexFormatted(input, ":")
		if serialBytes == nil {
			return nil, fmt.Errorf("error parsing serial %s", input)
		}
		ret.SetBytes(serialBytes)
	case strings.Count(input, "-") > 0:
		serialBytes := certutil.ParseHexFormatted(input, "-")
		if serialBytes == nil {
			return nil, fmt.Errorf("error parsing serial %s", input)
		}
		ret.SetBytes(serialBytes)
	default:
		var success bool
		ret, success = ret.SetString(input, 0)
		if !success {
			return nil, fmt.Errorf("error parsing serial %s", input)
		}
	}

	return ret, nil
}

func (b *backend) pathCRLDelete(
	req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	name := strings.ToLower(d.Get("name").(string))
	if name == "" {
		return logical.ErrorResponse(`"name" parameter cannot be empty`), nil
	}

	crlUpdateMutex.Lock()
	defer crlUpdateMutex.Unlock()

	_, ok := crls[name]
	if !ok {
		return logical.ErrorResponse(fmt.Sprintf(
			"no such CRL %s", name,
		)), nil
	}

	err := req.Storage.Delete("crls/" + name)
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf(
			"error deleting crl %s: %v", name, err),
		), nil
	}

	delete(crls, name)

	return nil, nil
}

func (b *backend) pathCRLRead(
	req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	name := strings.ToLower(d.Get("name").(string))
	serialStr := d.Get("serial").(string)
	if name == "" && serialStr == "" {
		return logical.ErrorResponse(`"name" or "serial" parameter must be set`), nil
	}

	crlUpdateMutex.RLock()
	defer crlUpdateMutex.RUnlock()

	var retData map[string]interface{}

	if serialStr != "" {
		serial, err := parseSerialString(serialStr)
		if err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}

		ret := findSerialInCRLs(serial)
		retData = structs.New(&ret).Map()
	} else {
		crl, ok := crls[name]
		if !ok {
			return logical.ErrorResponse(fmt.Sprintf(
				"no such CRL %s", name,
			)), nil
		}

		retData = structs.New(&crl).Map()
	}

	return &logical.Response{
		Data: retData,
	}, nil
}

func (b *backend) pathCRLWrite(
	req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	name := strings.ToLower(d.Get("name").(string))
	if name == "" {
		return logical.ErrorResponse(`"name" parameter cannot be empty`), nil
	}
	crl := d.Get("crl").(string)

	certList, err := x509.ParseCRL([]byte(crl))
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf("failed to parse CRL: %v", err)), nil
	}
	if certList == nil {
		return logical.ErrorResponse("parsed CRL is nil"), nil
	}

	crlUpdateMutex.Lock()
	defer crlUpdateMutex.Unlock()

	crlInfo := CRLInfo{
		Serials: map[string]RevokedSerialInfo{},
	}
	for _, revokedCert := range certList.TBSCertList.RevokedCertificates {
		crlInfo.Serials[revokedCert.SerialNumber.String()] = RevokedSerialInfo{}
	}

	entry, err := logical.StorageEntryJSON("crls/"+name, crlInfo)
	if err != nil {
		return nil, err
	}
	if err = req.Storage.Put(entry); err != nil {
		return nil, err
	}

	crls[name] = crlInfo

	return nil, nil
}

type CRLInfo struct {
	Serials map[string]RevokedSerialInfo `json:"serials" structs:"serials" mapstructure:"serials"`
}

type RevokedSerialInfo struct {
}

const pathCRLsHelpSyn = `
Manage Certificate Revocation Lists checked during authentication.
`

const pathCRLsHelpDesc = `
This endpoint allows you to create, read, update, and delete the Certificate
Revocation Lists checked during authentication.

When any CRLs are in effect, any login will check the trust chains sent by a
client against the submitted CRLs. Any chain containing a serial number revoked
by one or more of the CRLs causes that chain to be marked as invalid for the
authentication attempt. Conversely, *any* valid chain -- that is, a chain
in which none of the serials are revoked by any CRL -- allows authentication.
This allows authentication to succeed when interim parts of one chain have been
revoked; for instance, if a certificate is signed by two intermediate CAs due to
one of them expiring.
`

package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/voidmind-io/voidllm/internal/license"
)

// runLicense is the entry point for the "license" subcommand. It dispatches to
// keygen, generate, or verify based on the first positional argument.
func runLicense(args []string) {
	if len(args) == 0 {
		printLicenseUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "keygen":
		runLicenseKeygen(args[1:])
	case "generate":
		runLicenseGenerate(args[1:])
	case "verify":
		runLicenseVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "license: unknown subcommand %q\n", args[0])
		printLicenseUsage()
		os.Exit(1)
	}
}

func printLicenseUsage() {
	fmt.Println("Usage: voidllm license <subcommand> [flags]")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  keygen    Generate an Ed25519 keypair for license signing")
	fmt.Println("  generate  Sign a new license JWT using a private key")
	fmt.Println("  verify    Verify a license JWT using the embedded public key")
}

// runLicenseKeygen generates an Ed25519 keypair and writes them as PEM files.
func runLicenseKeygen(args []string) {
	fs := flag.NewFlagSet("license keygen", flag.ExitOnError)
	pubOut := fs.String("pub", "license_pub.pem", "Output path for the PEM-encoded public key")
	privOut := fs.String("priv", "license_priv.pem", "Output path for the PEM-encoded private key")
	fs.Parse(args) //nolint:errcheck // ExitOnError handles the error

	pub, priv, err := license.GenerateKeyPair()
	if err != nil {
		fmt.Fprintf(os.Stderr, "license keygen: %v\n", err)
		os.Exit(1)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "license keygen: marshal public key: %v\n", err)
		os.Exit(1)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "license keygen: marshal private key: %v\n", err)
		os.Exit(1)
	}

	if err := writePEM(*pubOut, "PUBLIC KEY", pubDER); err != nil {
		fmt.Fprintf(os.Stderr, "license keygen: write public key: %v\n", err)
		os.Exit(1)
	}
	if err := writePEM(*privOut, "PRIVATE KEY", privDER); err != nil {
		fmt.Fprintf(os.Stderr, "license keygen: write private key: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Public key:  %s\n", *pubOut)
	fmt.Printf("Private key: %s\n", *privOut)
	fmt.Println()
	fmt.Printf("Embedded public key (hex): %s\n", hex.EncodeToString(pub))
	fmt.Println()
	fmt.Println("Update internal/license/verify.go embeddedPublicKeyHex with the value above.")
}

// runLicenseGenerate reads a private key PEM file and signs a new license JWT.
func runLicenseGenerate(args []string) {
	fs := flag.NewFlagSet("license generate", flag.ExitOnError)
	privPath := fs.String("key", "license_priv.pem", "Path to PEM-encoded Ed25519 private key")
	customerID := fs.String("customer", "", "Customer identifier (required)")
	plan := fs.String("plan", "enterprise", "License plan name")
	featuresRaw := fs.String("features", strings.Join([]string{
		license.FeatureAuditLogs,
		license.FeatureOTelTracing,
		license.FeatureSSOOIDC,
		license.FeatureCustomRoles,
		license.FeatureMultiOrg,
	}, ","), "Comma-separated list of feature names to enable")
	maxOrgs := fs.Int("max-orgs", -1, "Maximum organizations (-1 for unlimited)")
	maxTeams := fs.Int("max-teams", -1, "Maximum teams (-1 for unlimited)")
	expiryDays := fs.Int("expires-days", 365, "License validity in days from now")
	fs.Parse(args) //nolint:errcheck // ExitOnError handles the error

	if *customerID == "" {
		fmt.Fprintln(os.Stderr, "license generate: --customer is required")
		os.Exit(1)
	}

	privPEM, err := os.ReadFile(*privPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "license generate: read key file: %v\n", err)
		os.Exit(1)
	}

	block, _ := pem.Decode(privPEM)
	if block == nil {
		fmt.Fprintln(os.Stderr, "license generate: no PEM block found in key file")
		os.Exit(1)
	}

	keyIface, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "license generate: parse private key: %v\n", err)
		os.Exit(1)
	}

	privKey, ok := keyIface.(ed25519.PrivateKey)
	if !ok {
		fmt.Fprintln(os.Stderr, "license generate: key file does not contain an Ed25519 private key")
		os.Exit(1)
	}

	features := []string{}
	for _, f := range strings.Split(*featuresRaw, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			features = append(features, f)
		}
	}

	now := time.Now().UTC()
	expiry := now.Add(time.Duration(*expiryDays) * 24 * time.Hour)

	claims := license.LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiry),
			Issuer:    "voidllm.ai",
		},
		Plan:       *plan,
		Features:   features,
		MaxOrgs:    *maxOrgs,
		MaxTeams:   *maxTeams,
		CustomerID: *customerID,
	}

	token, err := license.GenerateLicenseJWT(privKey, claims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "license generate: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(token)
}

// runLicenseVerify verifies a license JWT using the embedded public key.
func runLicenseVerify(args []string) {
	fs := flag.NewFlagSet("license verify", flag.ExitOnError)
	fs.Parse(args) //nolint:errcheck // ExitOnError handles the error

	var key string
	if fs.NArg() > 0 {
		key = fs.Arg(0)
	} else {
		// Read from stdin if no argument is provided. Cap at 64 KB to
		// prevent unbounded memory consumption from malicious or accidental
		// large inputs.
		const maxLicenseKeyBytes = 64 * 1024
		var sb strings.Builder
		buf := make([]byte, 4096)
		for sb.Len() < maxLicenseKeyBytes {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if readErr != nil {
				break
			}
		}
		key = strings.TrimSpace(sb.String())
	}

	if key == "" {
		fmt.Fprintln(os.Stderr, "license verify: no license key provided")
		fmt.Fprintln(os.Stderr, "Usage: voidllm license verify <jwt>")
		os.Exit(1)
	}

	lic := license.Verify(key, false)

	switch lic.Edition() {
	case license.EditionEnterprise:
		fmt.Printf("Edition:     %s\n", lic.Edition())
		fmt.Printf("Valid:       %v\n", lic.Valid())
		if !lic.ExpiresAt().IsZero() {
			fmt.Printf("Expires at:  %s\n", lic.ExpiresAt().Format(time.RFC3339))
		} else {
			fmt.Println("Expires at:  never")
		}
		fmt.Printf("Customer ID: %s\n", lic.CustomerID())
		fmt.Printf("Features:    %s\n", strings.Join(lic.Features(), ", "))
		fmt.Printf("Max orgs:    %d\n", lic.MaxOrgs())
		fmt.Printf("Max teams:   %d\n", lic.MaxTeams())
	default:
		fmt.Fprintln(os.Stderr, "license verify: key is invalid or expired — community edition will be used")
		os.Exit(1)
	}
}

// writePEM encodes DER bytes as a PEM block and writes it to path with mode 0600.
func writePEM(path, blockType string, der []byte) error {
	block := &pem.Block{Type: blockType, Bytes: der}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, block); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

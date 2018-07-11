package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"

	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/modules/wallet"
	"github.com/NebulousLabs/Sia/types"
	"github.com/NebulousLabs/entropy-mnemonics"
)

var (
	walletAddressCmd = &cobra.Command{
		Use:   "address",
		Short: "Get a new wallet address",
		Long:  "Generate a new wallet address from the wallet's primary seed.",
		Run:   wrap(walletaddresscmd),
	}

	walletAddressesCmd = &cobra.Command{
		Use:   "addresses",
		Short: "List all addresses",
		Long:  "List all addresses that have been generated by the wallet.",
		Run:   wrap(walletaddressescmd),
	}

	walletBalanceCmd = &cobra.Command{
		Use:   "balance",
		Short: "View wallet balance",
		Long:  "View wallet balance, including confirmed and unconfirmed siacoins and siafunds.",
		Run:   wrap(walletbalancecmd),
	}

	walletChangepasswordCmd = &cobra.Command{
		Use:   "change-password",
		Short: "Change the wallet password",
		Long:  "Change the encryption password of the wallet, re-encrypting all keys + seeds kept by the wallet.",
		Run:   wrap(walletchangepasswordcmd),
	}

	walletCmd = &cobra.Command{
		Use:   "wallet",
		Short: "Perform wallet actions",
		Long: `Generate a new address, send coins to another wallet, or view info about the wallet.

Units:
The smallest unit of siacoins is the hasting. One siacoin is 10^24 hastings. Other supported units are:
  pS (pico,  10^-12 SC)
  nS (nano,  10^-9 SC)
  uS (micro, 10^-6 SC)
  mS (milli, 10^-3 SC)
  SC
  KS (kilo, 10^3 SC)
  MS (mega, 10^6 SC)
  GS (giga, 10^9 SC)
  TS (tera, 10^12 SC)`,
		Run: wrap(walletbalancecmd),
	}

	walletInitCmd = &cobra.Command{
		Use:   "init",
		Short: "Initialize and encrypt a new wallet",
		Long: `Generate a new wallet from a randomly generated seed, and encrypt it.
By default the wallet encryption / unlock password is the same as the generated seed.`,
		Run: wrap(walletinitcmd),
	}

	walletInitSeedCmd = &cobra.Command{
		Use:   "init-seed",
		Short: "Initialize and encrypt a new wallet using a pre-existing seed",
		Long:  `Initialize and encrypt a new wallet using a pre-existing seed.`,
		Run:   wrap(walletinitseedcmd),
	}

	walletLoad033xCmd = &cobra.Command{
		Use:   "033x [filepath]",
		Short: "Load a v0.3.3.x wallet",
		Long:  "Load a v0.3.3.x wallet into the current wallet",
		Run:   wrap(walletload033xcmd),
	}

	walletLoadCmd = &cobra.Command{
		Use:   "load",
		Short: "Load a wallet seed, v0.3.3.x wallet, or siag keyset",
		// Run field is not set, as the load command itself is not a valid command.
		// A subcommand must be provided.
	}

	walletLoadSeedCmd = &cobra.Command{
		Use:   `seed`,
		Short: "Add a seed to the wallet",
		Long:  "Loads an auxiliary seed into the wallet.",
		Run:   wrap(walletloadseedcmd),
	}

	walletLoadSiagCmd = &cobra.Command{
		Use:     `siag [filepath,...]`,
		Short:   "Load siag key(s) into the wallet",
		Long:    "Load siag key(s) into the wallet - typically used for siafunds.",
		Example: "siac wallet load siag key1.siakey,key2.siakey",
		Run:     wrap(walletloadsiagcmd),
	}

	walletLockCmd = &cobra.Command{
		Use:   "lock",
		Short: "Lock the wallet",
		Long:  "Lock the wallet, preventing further use",
		Run:   wrap(walletlockcmd),
	}

	walletSeedsCmd = &cobra.Command{
		Use:   "seeds",
		Short: "View information about your seeds",
		Long:  "View your primary and auxiliary wallet seeds.",
		Run:   wrap(walletseedscmd),
	}

	walletSendCmd = &cobra.Command{
		Use:   "send",
		Short: "Send either siacoins or siafunds to an address",
		Long:  "Send either siacoins or siafunds to an address",
		// Run field is not set, as the send command itself is not a valid command.
		// A subcommand must be provided.
	}

	walletSendSiacoinsCmd = &cobra.Command{
		Use:   "siacoins [amount] [dest]",
		Short: "Send siacoins to an address",
		Long: `Send siacoins to an address. 'dest' must be a 76-byte hexadecimal address.
'amount' can be specified in units, e.g. 1.23KS. Run 'wallet --help' for a list of units.
If no unit is supplied, hastings will be assumed.

A dynamic transaction fee is applied depending on the size of the transaction and how busy the network is.`,
		Run: wrap(walletsendsiacoinscmd),
	}

	walletSendSiafundsCmd = &cobra.Command{
		Use:   "siafunds [amount] [dest]",
		Short: "Send siafunds",
		Long: `Send siafunds to an address, and transfer the claim siacoins to your wallet.
Run 'wallet send --help' to see a list of available units.`,
		Run: wrap(walletsendsiafundscmd),
	}

	walletSignCmd = &cobra.Command{
		Use:   "sign [txn] [tosign]",
		Short: "Sign a transaction",
		Long: `Sign the specified inputs of a transaction. If siad is running with an
unlocked wallet, the /wallet/sign API call will be used. Otherwise, sign will
prompt for the wallet seed, and the signing key(s) will be regenerated.`,
		Run: wrap(walletsigncmd),
	}

	walletSweepCmd = &cobra.Command{
		Use:   "sweep",
		Short: "Sweep siacoins and siafunds from a seed.",
		Long: `Sweep siacoins and siafunds from a seed. The outputs belonging to the seed
will be sent to your wallet.`,
		Run: wrap(walletsweepcmd),
	}

	walletTransactionsCmd = &cobra.Command{
		Use:   "transactions",
		Short: "View transactions",
		Long:  "View transactions related to addresses spendable by the wallet, providing a net flow of siacoins and siafunds for each transaction",
		Run:   wrap(wallettransactionscmd),
	}

	walletUnlockCmd = &cobra.Command{
		Use:   `unlock`,
		Short: "Unlock the wallet",
		Long: `Decrypt and load the wallet into memory.
Automatic unlocking is also supported via environment variable: if the
SIA_WALLET_PASSWORD environment variable is set, the unlock command will
use it instead of displaying the typical interactive prompt.`,
		Run: wrap(walletunlockcmd),
	}
)

const askPasswordText = "We need to encrypt the new data using the current wallet password, please provide: "

const currentPasswordText = "Current Password: "
const newPasswordText = "New Password: "
const confirmPasswordText = "Confirm: "

// For an unconfirmed Transaction, the TransactionTimestamp field is set to the
// maximum value of a uint64.
const unconfirmedTransactionTimestamp = ^uint64(0)

// passwordPrompt securely reads a password from stdin.
func passwordPrompt(prompt string) (string, error) {
	fmt.Print(prompt)
	pw, err := terminal.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	return string(pw), err
}

// confirmPassword requests confirmation of a previously-entered password.
func confirmPassword(prev string) error {
	pw, err := passwordPrompt(confirmPasswordText)
	if err != nil {
		return err
	} else if pw != prev {
		return errors.New("passwords do not match")
	}
	return nil
}

// walletaddresscmd fetches a new address from the wallet that will be able to
// receive coins.
func walletaddresscmd() {
	addr, err := httpClient.WalletAddressGet()
	if err != nil {
		die("Could not generate new address:", err)
	}
	fmt.Printf("Created new address: %s\n", addr.Address)
}

// walletaddressescmd fetches the list of addresses that the wallet knows.
func walletaddressescmd() {
	addrs, err := httpClient.WalletAddressesGet()
	if err != nil {
		die("Failed to fetch addresses:", err)
	}
	for _, addr := range addrs.Addresses {
		fmt.Println(addr)
	}
}

// walletchangepasswordcmd changes the password of the wallet.
func walletchangepasswordcmd() {
	currentPassword, err := passwordPrompt(currentPasswordText)
	if err != nil {
		die("Reading password failed:", err)
	}
	newPassword, err := passwordPrompt(newPasswordText)
	if err != nil {
		die("Reading password failed:", err)
	} else if err = confirmPassword(newPassword); err != nil {
		die(err)
	}
	err = httpClient.WalletChangePasswordPost(currentPassword, newPassword)
	if err != nil {
		die("Changing the password failed:", err)
	}
	fmt.Println("Password changed successfully.")
}

// walletinitcmd encrypts the wallet with the given password
func walletinitcmd() {
	var password string
	var err error
	if initPassword {
		password, err = passwordPrompt("Wallet password: ")
		if err != nil {
			die("Reading password failed:", err)
		} else if err = confirmPassword(password); err != nil {
			die(err)
		}
	}
	er, err := httpClient.WalletInitPost(password, initForce)
	if err != nil {
		die("Error when encrypting wallet:", err)
	}
	fmt.Printf("Recovery seed:\n%s\n\n", er.PrimarySeed)
	if initPassword {
		fmt.Printf("Wallet encrypted with given password\n")
	} else {
		fmt.Printf("Wallet encrypted with password:\n%s\n", er.PrimarySeed)
	}
}

// walletinitseedcmd initializes the wallet from a preexisting seed.
func walletinitseedcmd() {
	seed, err := passwordPrompt("Seed: ")
	if err != nil {
		die("Reading seed failed:", err)
	}
	var password string
	if initPassword {
		password, err = passwordPrompt("Wallet password: ")
		if err != nil {
			die("Reading password failed:", err)
		} else if err = confirmPassword(password); err != nil {
			die(err)
		}
	}
	err = httpClient.WalletInitSeedPost(seed, password, initForce)
	if err != nil {
		die("Could not initialize wallet from seed:", err)
	}
	if initPassword {
		fmt.Println("Wallet initialized and encrypted with given password.")
	} else {
		fmt.Println("Wallet initialized and encrypted with seed.")
	}
}

// walletload033xcmd loads a v0.3.3.x wallet into the current wallet.
func walletload033xcmd(source string) {
	password, err := passwordPrompt(askPasswordText)
	if err != nil {
		die("Reading password failed:", err)
	}
	err = httpClient.Wallet033xPost(abs(source), password)
	if err != nil {
		die("Loading wallet failed:", err)
	}
	fmt.Println("Wallet loading successful.")
}

// walletloadseedcmd adds a seed to the wallet's list of seeds
func walletloadseedcmd() {
	seed, err := passwordPrompt("New seed: ")
	if err != nil {
		die("Reading seed failed:", err)
	}
	password, err := passwordPrompt(askPasswordText)
	if err != nil {
		die("Reading password failed:", err)
	}
	err = httpClient.WalletSeedPost(seed, password)
	if err != nil {
		die("Could not add seed:", err)
	}
	fmt.Println("Added Key")
}

// walletloadsiagcmd loads a siag key set into the wallet.
func walletloadsiagcmd(keyfiles string) {
	password, err := passwordPrompt(askPasswordText)
	if err != nil {
		die("Reading password failed:", err)
	}
	err = httpClient.WalletSiagKeyPost(keyfiles, password)
	if err != nil {
		die("Loading siag key failed:", err)
	}
	fmt.Println("Wallet loading successful.")
}

// walletlockcmd locks the wallet
func walletlockcmd() {
	err := httpClient.WalletLockPost()
	if err != nil {
		die("Could not lock wallet:", err)
	}
}

// walletseedcmd returns the current seed {
func walletseedscmd() {
	seedInfo, err := httpClient.WalletSeedsGet()
	if err != nil {
		die("Error retrieving the current seed:", err)
	}
	fmt.Println("Primary Seed:")
	fmt.Println(seedInfo.PrimarySeed)
	if len(seedInfo.AllSeeds) == 1 {
		// AllSeeds includes the primary seed
		return
	}
	fmt.Println()
	fmt.Println("Auxiliary Seeds:")
	for _, seed := range seedInfo.AllSeeds {
		if seed == seedInfo.PrimarySeed {
			continue
		}
		fmt.Println() // extra newline for readability
		fmt.Println(seed)
	}
}

// walletsendsiacoinscmd sends siacoins to a destination address.
func walletsendsiacoinscmd(amount, dest string) {
	hastings, err := parseCurrency(amount)
	if err != nil {
		die("Could not parse amount:", err)
	}
	var value types.Currency
	if _, err := fmt.Sscan(hastings, &value); err != nil {
		die("Failed to parse amount", err)
	}
	var hash types.UnlockHash
	if _, err := fmt.Sscan(dest, &hash); err != nil {
		die("Failed to parse destination address", err)
	}
	_, err = httpClient.WalletSiacoinsPost(value, hash)
	if err != nil {
		die("Could not send siacoins:", err)
	}
	fmt.Printf("Sent %s hastings to %s\n", hastings, dest)
}

// walletsendsiafundscmd sends siafunds to a destination address.
func walletsendsiafundscmd(amount, dest string) {
	var value types.Currency
	if _, err := fmt.Sscan(amount, &value); err != nil {
		die("Failed to parse amount", err)
	}
	var hash types.UnlockHash
	if _, err := fmt.Sscan(dest, &hash); err != nil {
		die("Failed to parse destination address", err)
	}
	_, err := httpClient.WalletSiafundsPost(value, hash)
	if err != nil {
		die("Could not send siafunds:", err)
	}
	fmt.Printf("Sent %s siafunds to %s\n", amount, dest)
}

// walletbalancecmd retrieves and displays information about the wallet.
func walletbalancecmd() {
	status, err := httpClient.WalletGet()
	if err != nil {
		die("Could not get wallet status:", err)
	}
	fees, err := httpClient.TransactionPoolFeeGet()
	if err != nil {
		die("Could not get fee estimation:", err)
	}
	encStatus := "Unencrypted"
	if status.Encrypted {
		encStatus = "Encrypted"
	}
	if !status.Unlocked {
		fmt.Printf(`Wallet status:
%v, Locked
Unlock the wallet to view balance
`, encStatus)
		return
	}

	unconfirmedBalance := status.ConfirmedSiacoinBalance.Add(status.UnconfirmedIncomingSiacoins).Sub(status.UnconfirmedOutgoingSiacoins)
	var delta string
	if unconfirmedBalance.Cmp(status.ConfirmedSiacoinBalance) >= 0 {
		delta = "+" + currencyUnits(unconfirmedBalance.Sub(status.ConfirmedSiacoinBalance))
	} else {
		delta = "-" + currencyUnits(status.ConfirmedSiacoinBalance.Sub(unconfirmedBalance))
	}

	fmt.Printf(`Wallet status:
%s, Unlocked
Height:              %v
Confirmed Balance:   %v
Unconfirmed Delta:  %v
Exact:               %v H
Siafunds:            %v SF
Siafund Claims:      %v H

Estimated Fee:       %v / KB
`, encStatus, status.Height, currencyUnits(status.ConfirmedSiacoinBalance), delta,
		status.ConfirmedSiacoinBalance, status.SiafundBalance, status.SiacoinClaimBalance,
		fees.Maximum.Mul64(1e3).HumanString())
}

// walletsweepcmd sweeps coins and funds from a seed.
func walletsweepcmd() {
	seed, err := passwordPrompt("Seed: ")
	if err != nil {
		die("Reading seed failed:", err)
	}

	swept, err := httpClient.WalletSweepPost(seed)
	if err != nil {
		die("Could not sweep seed:", err)
	}
	fmt.Printf("Swept %v and %v SF from seed.\n", currencyUnits(swept.Coins), swept.Funds)
}

// walletsigncmd signs a transaction.
func walletsigncmd(txnJSON, toSignJSON string) {
	var txn types.Transaction
	err := json.Unmarshal([]byte(txnJSON), &txn)
	if err != nil {
		die("Invalid transaction:", err)
	}

	var toSign map[types.OutputID]types.UnlockHash
	err = json.Unmarshal([]byte(toSignJSON), &toSign)
	if err != nil {
		die("Invalid transaction:", err)
	}

	// try API first
	wspr, err := httpClient.WalletSignPost(txn, toSign)
	if err == nil {
		txn = wspr.Transaction
	} else {
		// fallback to offline keygen
		fmt.Println("Signing via API failed: either siad is not running, or your wallet is locked.")
		fmt.Println("Enter your wallet seed to generate the signing key(s) now and sign without siad.")
		seedString, err := passwordPrompt("Seed: ")
		if err != nil {
			die("Reading seed failed:", err)
		}
		seed, err := modules.StringToSeed(seedString, mnemonics.English)
		if err != nil {
			die("Invalid seed:", err)
		}
		err = wallet.SignTransaction(&txn, seed, toSign)
		if err != nil {
			die("Failed to sign transaction:", err)
		}
	}

	if walletSignRaw {
		base64.NewEncoder(base64.StdEncoding, os.Stdout).Write(encoding.Marshal(txn))
	} else {
		json.NewEncoder(os.Stdout).Encode(txn)
	}
	fmt.Println()
}

// wallettransactionscmd lists all of the transactions related to the wallet,
// providing a net flow of siacoins and siafunds for each.
func wallettransactionscmd() {
	wtg, err := httpClient.WalletTransactionsGet(0, math.MaxInt64)
	if err != nil {
		die("Could not fetch transaction history:", err)
	}
	fmt.Println("             [timestamp]    [height]                                                   [transaction id]    [net siacoins]   [net siafunds]")
	txns := append(wtg.ConfirmedTransactions, wtg.UnconfirmedTransactions...)
	for _, txn := range txns {
		// Determine the number of outgoing siacoins and siafunds.
		var outgoingSiacoins types.Currency
		var outgoingSiafunds types.Currency
		for _, input := range txn.Inputs {
			if input.FundType == types.SpecifierSiacoinInput && input.WalletAddress {
				outgoingSiacoins = outgoingSiacoins.Add(input.Value)
			}
			if input.FundType == types.SpecifierSiafundInput && input.WalletAddress {
				outgoingSiafunds = outgoingSiafunds.Add(input.Value)
			}
		}

		// Determine the number of incoming siacoins and siafunds.
		var incomingSiacoins types.Currency
		var incomingSiafunds types.Currency
		for _, output := range txn.Outputs {
			if output.FundType == types.SpecifierMinerPayout {
				incomingSiacoins = incomingSiacoins.Add(output.Value)
			}
			if output.FundType == types.SpecifierSiacoinOutput && output.WalletAddress {
				incomingSiacoins = incomingSiacoins.Add(output.Value)
			}
			if output.FundType == types.SpecifierSiafundOutput && output.WalletAddress {
				incomingSiafunds = incomingSiafunds.Add(output.Value)
			}
		}

		// Convert the siacoins to a float.
		incomingSiacoinsFloat, _ := new(big.Rat).SetFrac(incomingSiacoins.Big(), types.SiacoinPrecision.Big()).Float64()
		outgoingSiacoinsFloat, _ := new(big.Rat).SetFrac(outgoingSiacoins.Big(), types.SiacoinPrecision.Big()).Float64()

		// Print the results.
		if uint64(txn.ConfirmationTimestamp) != unconfirmedTransactionTimestamp {
			fmt.Printf(time.Unix(int64(txn.ConfirmationTimestamp), 0).Format("2006-01-02 15:04:05-0700"))
		} else {
			fmt.Printf("             unconfirmed")
		}
		if txn.ConfirmationHeight < 1e9 {
			fmt.Printf("%12v", txn.ConfirmationHeight)
		} else {
			fmt.Printf(" unconfirmed")
		}
		fmt.Printf("%67v%15.2f SC", txn.TransactionID, incomingSiacoinsFloat-outgoingSiacoinsFloat)
		// For siafunds, need to avoid having a negative types.Currency.
		if incomingSiafunds.Cmp(outgoingSiafunds) >= 0 {
			fmt.Printf("%14v SF\n", incomingSiafunds.Sub(outgoingSiafunds))
		} else {
			fmt.Printf("-%14v SF\n", outgoingSiafunds.Sub(incomingSiafunds))
		}
	}
}

// walletunlockcmd unlocks a saved wallet
func walletunlockcmd() {
	// try reading from environment variable first, then fallback to
	// interactive method. Also allow overriding auto-unlock via -p
	password := os.Getenv("SIA_WALLET_PASSWORD")
	if password != "" && !initPassword {
		fmt.Println("Using SIA_WALLET_PASSWORD environment variable")
		err := httpClient.WalletUnlockPost(password)
		if err != nil {
			fmt.Println("Automatic unlock failed!")
		} else {
			fmt.Println("Wallet unlocked")
			return
		}
	}
	password, err := passwordPrompt("Wallet password: ")
	if err != nil {
		die("Reading password failed:", err)
	}
	err = httpClient.WalletUnlockPost(password)
	if err != nil {
		die("Could not unlock wallet:", err)
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/siad/v2/api/siad"
	"golang.org/x/crypto/ssh/terminal"

	"lukechampine.com/flagg"
)

var (
	// to be supplied at build time
	githash   = "?"
	builddate = "?"
)

var (
	rootUsage = `Usage:
    siac [flags] [action|subcommand]

Actions:
    version         display version information

Subcommands:
    wallet
    txpool
    syncer
`
	versionUsage = rootUsage

	walletUsage = `Usage:
    siac [flags] wallet [flags] [action]

Actions:
    address         generate a wallet address
    addresses       list wallet addresses
    balance         view current balance
    transactions    list wallet-related transactions
    sign            sign a transaction
`

	walletBalanceUsage = `Usage:
    siac [flags] wallet [flags] balance

View the current wallet balance.
`
	walletAddressUsage = `Usage:
    siac [flags] wallet [flags] address

Generate a new wallet address.
`
	walletAddressesUsage = `Usage:
    siac [flags] wallet [flags] addresses

List addresses generated by the wallet.
`

	walletTransactionsUsage = `Usage:
    siac [flags] wallet [flags] transactions

List transactions relevant to the wallet.
`

	walletSignUsage = `Usage:
    siac [flags] wallet [flags] sign [file]

Signs all wallet-controlled inputs of the provided JSON-encoded transaction.
The result is written to a new file.
`

	txpoolUsage = `Usage:
    siac [flags] txpool [flags] [action]

Actions:
    transactions    display all transactions in the transaction pool
    broadcast       broadcast a JSON encoded transaction
`

	txpoolBroadcastUsage = `Usage:
    siac [flags] txpool [flags] broadcast [file]

Broadcast a JSON encoded transaction.
`

	txpoolTransactionsUsage = `Usage:
    siac [flags] txpool [flags] transactions

List transactions in the transaction pool.
`

	syncerUsage = `Usage:
    siac [flags] syncer [flags] [action]

Actions:
    peers           display all the syncer's peers
    connect         add a peer to the syncer
`

	syncerPeersUsage = `Usage:
    siac [flags] syncer [flags] peers

List current peers.
`

	syncerConnectUsage = `Usage:
    siac [flags] syncer [flags] connect [addr]

Add the provided address as a peer.
`
)

func check(ctx string, err error) {
	if err != nil {
		log.Fatalln(ctx, err)
	}
}

var siadAddr string

func getClient() *siad.Client {
	password := getAPIPassword()
	if !strings.HasPrefix(siadAddr, "https://") && !strings.HasPrefix(siadAddr, "http://") {
		siadAddr = "http://" + siadAddr
	}
	c := siad.NewClient(siadAddr, password)
	_, err := c.WalletBalance()
	check("Couldn't connect to siad:", err)
	return c
}

func makeTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(
		os.Stdout,
		0,   // minwidth: zero, since we're left-aligning
		0,   // tabwidth: zero, since we're using ' ',  not '\t'
		2,   // padding:  two, so that columns aren't right next to each other
		' ', // padchar:  spaces, not tabs
		0,   // flags:    none
	)
}

func readTxn(filename string) types.Transaction {
	js, err := os.ReadFile(filename)
	check("Could not read transaction file", err)
	var txn types.Transaction
	err = json.Unmarshal(js, &txn)
	check("Could not parse transaction file", err)
	return txn
}

func writeTxn(filename string, txn types.Transaction) {
	js, _ := json.MarshalIndent(txn, "", "  ")
	js = append(js, '\n')
	err := os.WriteFile(filename, js, 0666)
	check("Could not write transaction to disk", err)
}

var getAPIPassword = func() func() string {
	return func() string {
		apiPassword := os.Getenv("SIAD_API_PASSWORD")
		if len(apiPassword) == 0 {
			fmt.Print("Enter API password: ")
			pw, err := terminal.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				log.Fatal(err)
			}
			apiPassword = string(pw)
		} else {
			fmt.Println("Using SIAD_API_PASSWORD environment variable.")
		}
		return apiPassword
	}
}()

func main() {
	log.SetFlags(0)
	var verbose, exactCurrency, broadcast bool

	rootCmd := flagg.Root
	rootCmd.Usage = flagg.SimpleUsage(rootCmd, rootUsage)
	rootCmd.StringVar(&siadAddr, "a", "localhost:9980", "siad API server address")
	rootCmd.BoolVar(&verbose, "v", false, "print verbose output")

	versionCmd := flagg.New("version", versionUsage)

	walletCmd := flagg.New("wallet", walletUsage)
	walletBalanceCmd := flagg.New("balance", walletBalanceUsage)
	walletBalanceCmd.BoolVar(&exactCurrency, "exact", false, "print balance in Hastings")
	walletAddressCmd := flagg.New("address", walletAddressUsage)
	walletAddressesCmd := flagg.New("addresses", walletAddressesUsage)
	walletTransactionsCmd := flagg.New("transactions", walletTransactionsUsage)
	walletSignCmd := flagg.New("sign", walletSignUsage)
	walletSignCmd.BoolVar(&broadcast, "broadcast", false, "immediately broadcast the transaction")

	txpoolCmd := flagg.New("txpool", txpoolUsage)
	txpoolBroadcastCmd := flagg.New("broadcast", txpoolBroadcastUsage)
	txpoolTransactionsCmd := flagg.New("transactions", txpoolTransactionsUsage)

	syncerCmd := flagg.New("syncer", syncerUsage)
	syncerPeersCmd := flagg.New("peers", syncerPeersUsage)
	syncerConnectCmd := flagg.New("connect", syncerConnectUsage)

	cmd := flagg.Parse(flagg.Tree{
		Cmd: rootCmd,
		Sub: []flagg.Tree{
			{Cmd: versionCmd},
			{
				Cmd: walletCmd,
				Sub: []flagg.Tree{
					{Cmd: walletAddressCmd},
					{Cmd: walletAddressesCmd},
					{Cmd: walletBalanceCmd},
					{Cmd: walletTransactionsCmd},
					{Cmd: walletSignCmd},
				},
			},
			{
				Cmd: txpoolCmd,
				Sub: []flagg.Tree{
					{Cmd: txpoolTransactionsCmd},
					{Cmd: txpoolBroadcastCmd},
				},
			},
			{
				Cmd: syncerCmd,
				Sub: []flagg.Tree{
					{Cmd: syncerPeersCmd},
					{Cmd: syncerConnectCmd},
				},
			},
		},
	})
	args := cmd.Args()

	switch cmd {
	case rootCmd:
		if len(args) != 0 {
			cmd.Usage()
			return
		}
		fallthrough
	case versionCmd:
		log.Printf("siac v2.0.0\nCommit:     %s\nGo version: %s %s/%s\nBuild Date: %s\n",
			githash, runtime.Version(), runtime.GOOS, runtime.GOARCH, builddate)

	case walletCmd:
		cmd.Usage()

	case walletAddressCmd:
		if len(args) != 0 {
			cmd.Usage()
			return
		}
		address, err := getClient().WalletAddress()
		check("Couldn't get address:", err)
		fmt.Println(address.Address)

	case walletAddressesCmd:
		if len(args) != 0 {
			cmd.Usage()
			return
		}
		addresses, err := getClient().WalletAddresses(0, math.MaxInt64)
		check("Couldn't get address:", err)
		for _, address := range addresses {
			fmt.Println(address)
		}

	case walletBalanceCmd:
		if len(args) != 0 {
			cmd.Usage()
			return
		}
		balance, err := getClient().WalletBalance()
		check("Couldn't get balance:", err)
		if *&exactCurrency {
			fmt.Printf("%d H\n", balance.Siacoins)
		} else {
			fmt.Printf("%s SC\n", balance.Siacoins)
		}

	case walletTransactionsCmd:
		if len(args) != 0 {
			cmd.Usage()
			return
		}
		transactions, err := getClient().WalletTransactions(time.Time{}, 0)
		check("Couldn't get transactions:", err)

		w := makeTabWriter()
		defer w.Flush()
		fmt.Fprintf(w, "%v\n", "ID")
		for _, wt := range transactions {
			txn := wt.Transaction
			fmt.Fprintf(w, "%v\n", txn.ID())
		}

	case walletSignCmd:
		if len(args) != 1 {
			cmd.Usage()
			return
		}

		txn := readTxn(args[0])
		err := getClient().WalletSign(&txn, nil)
		check("failed to sign transaction:", err)
		if broadcast {
			err = getClient().TxpoolBroadcast(txn, nil)
			check("failed to broadcast transaction:", err)
			fmt.Println("Signed and broadcast transaction.")
		} else {
			ext := filepath.Ext(args[0])
			signedPath := strings.TrimSuffix(args[0], ext) + "-signed" + ext
			writeTxn(signedPath, txn)
			fmt.Printf("Wrote signed transaction to %v.\n", signedPath)
		}

	case txpoolCmd:
		cmd.Usage()

	case txpoolBroadcastCmd:
		if len(args) != 1 {
			cmd.Usage()
			return
		}

		txn := readTxn(args[0])
		err := getClient().TxpoolBroadcast(txn, nil)
		check("failed to broadcast transaction:", err)
		fmt.Println("Broadcast transaction.")

	case txpoolTransactionsCmd:
		if len(args) != 0 {
			cmd.Usage()
			return
		}

		transactions, err := getClient().TxpoolTransactions()
		check("Couldn't get transactions:", err)

		w := makeTabWriter()
		defer w.Flush()
		fmt.Fprintf(w, "%v\n", "ID")
		for _, txn := range transactions {
			fmt.Fprintf(w, "%v\n", txn.ID())
		}

	case syncerCmd:
		cmd.Usage()

	case syncerPeersCmd:
		if len(args) != 0 {
			cmd.Usage()
			return
		}
		peers, err := getClient().SyncerPeers()
		check("Couldn't get peers:", err)

		w := makeTabWriter()
		defer w.Flush()
		fmt.Fprintf(w, "%v\n", "NetAddress")
		for _, peer := range peers {
			fmt.Fprintf(w, "%v\n", peer.NetAddress)
		}

	case syncerConnectCmd:
		if len(args) != 1 {
			cmd.Usage()
			return
		}
		addr := args[0]
		err := getClient().SyncerConnect(addr)
		check("Couldn't connect:", err)
		fmt.Printf("Connected to %v.\n", addr)
	}
}

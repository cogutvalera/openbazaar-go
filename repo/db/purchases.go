package db

import (
	"database/sql"
	"encoding/json"
	"github.com/OpenBazaar/jsonpb"
	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/openbazaar-go/repo"
	"github.com/OpenBazaar/spvwallet"
	btc "github.com/btcsuite/btcutil"
	"sync"
	"time"
)

type PurchasesDB struct {
	db   *sql.DB
	lock sync.RWMutex
}

func (p *PurchasesDB) Put(orderID string, contract pb.RicardianContract, state pb.OrderState, read bool) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	readInt := 0
	if read {
		readInt = 1
	}
	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: true,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(&contract)

	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	stm := `insert or replace into purchases(orderID, contract, state, read, timestamp, total, thumbnail, vendorID, vendorBlockchainID, title, shippingName, shippingAddress, paymentAddr, funded, transactions) values(?,?,?,?,?,?,?,?,?,?,?,?,?,(select funded from purchases where orderID="` + orderID + `"),(select transactions from purchases where orderID="` + orderID + `"))`
	stmt, err := tx.Prepare(stm)
	if err != nil {
		return err
	}
	blockchainID := contract.VendorListings[0].VendorID.BlockchainID
	shippingName := ""
	shippingAddress := ""
	if contract.BuyerOrder.Shipping != nil {
		shippingName = contract.BuyerOrder.Shipping.ShipTo
		shippingAddress = contract.BuyerOrder.Shipping.Address
	}
	var paymentAddr string
	if contract.BuyerOrder.Payment.Method == pb.Order_Payment_DIRECT || contract.BuyerOrder.Payment.Method == pb.Order_Payment_MODERATED {
		paymentAddr = contract.BuyerOrder.Payment.Address
	} else if contract.BuyerOrder.Payment.Method == pb.Order_Payment_ADDRESS_REQUEST {
		paymentAddr = contract.VendorOrderConfirmation.PaymentAddress
	}
	defer stmt.Close()
	_, err = stmt.Exec(
		orderID,
		out,
		int(state),
		readInt,
		int(contract.BuyerOrder.Timestamp.Seconds),
		int(contract.BuyerOrder.Payment.Amount),
		contract.VendorListings[0].Item.Images[0].Tiny,
		contract.VendorListings[0].VendorID.PeerID,
		blockchainID,
		contract.VendorListings[0].Item.Title,
		shippingName,
		shippingAddress,
		paymentAddr,
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	tx.Commit()
	return nil
}

func (p *PurchasesDB) MarkAsRead(orderID string) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	_, err := p.db.Exec("update purchases set read=? where orderID=?", 1, orderID)
	if err != nil {
		return err
	}
	return nil
}

func (p *PurchasesDB) UpdateFunding(orderId string, funded bool, records []*spvwallet.TransactionRecord) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	fundedInt := 0
	if funded {
		fundedInt = 1
	}
	serializedTransactions, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = p.db.Exec("update purchases set funded=?, transactions=? where orderID=?", fundedInt, string(serializedTransactions), orderId)
	if err != nil {
		return err
	}
	return nil
}

func (p *PurchasesDB) Delete(orderID string) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	_, err := p.db.Exec("delete from purchases where orderID=?", orderID)
	if err != nil {
		return err
	}
	return nil
}

func (p *PurchasesDB) GetAll(offsetId string, limit int, stateFilter []pb.OrderState, searchTerm string, ascending bool) ([]repo.Purchase, int, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	q := query{
		table:         "purchases",
		columns:       []string{"orderID", "timestamp", "total", "title", "thumbnail", "vendorID", "vendorBlockchainID", "shippingName", "shippingAddress", "state", "read"},
		offsetId:      offsetId,
		stateFilter:   stateFilter,
		searchTerm:    searchTerm,
		searchColumns: []string{"orderID", "timestamp", "total", "title", "thumbnail", "vendorID", "vendorBlockchainID", "shippingName", "shippingAddress", "paymentAddr"},
		ascending:     ascending,
		limit:         limit,
	}
	stm, args := filterQuery(q)
	rows, err := p.db.Query(stm, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var ret []repo.Purchase
	for rows.Next() {
		var orderID, title, thumbnail, vendorID, vendorHandle, shippingName, shippingAddr string
		var timestamp, total, stateInt, readInt int
		if err := rows.Scan(&orderID, &timestamp, &total, &title, &thumbnail, &vendorID, &vendorHandle, &shippingName, &shippingAddr, &stateInt, &readInt); err != nil {
			return ret, 0, err
		}
		read := false
		if readInt > 0 {
			read = true
		}

		ret = append(ret, repo.Purchase{
			OrderId:         orderID,
			Timestamp:       time.Unix(int64(timestamp), 0),
			Title:           title,
			Thumbnail:       thumbnail,
			Total:           uint64(total),
			VendorId:        vendorID,
			VendorHandle:    vendorHandle,
			ShippingName:    shippingName,
			ShippingAddress: shippingAddr,
			State:           pb.OrderState(stateInt).String(),
			Read:            read,
		})
	}
	q.columns = []string{"Count(*)"}
	q.offsetId = ""
	q.limit = -1
	stm, args = filterQuery(q)
	row := p.db.QueryRow(stm, args...)
	var count int
	err = row.Scan(&count)
	if err != nil {
		return ret, 0, err
	}
	return ret, count, nil
}

func (p *PurchasesDB) GetByPaymentAddress(addr btc.Address) (*pb.RicardianContract, pb.OrderState, bool, []*spvwallet.TransactionRecord, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	stmt, err := p.db.Prepare("select contract, state, funded, transactions from purchases where paymentAddr=?")
	defer stmt.Close()
	var contract []byte
	var stateInt int
	var fundedInt *int
	var serializedTransactions []byte
	err = stmt.QueryRow(addr.EncodeAddress()).Scan(&contract, &stateInt, &fundedInt, &serializedTransactions)
	if err != nil {
		return nil, pb.OrderState(0), false, nil, err
	}
	rc := new(pb.RicardianContract)
	err = jsonpb.UnmarshalString(string(contract), rc)
	if err != nil {
		return nil, pb.OrderState(0), false, nil, err
	}
	funded := false
	if fundedInt != nil && *fundedInt == 1 {
		funded = true
	}
	var records []*spvwallet.TransactionRecord
	json.Unmarshal(serializedTransactions, &records)
	return rc, pb.OrderState(stateInt), funded, records, nil
}

func (p *PurchasesDB) GetByOrderId(orderId string) (*pb.RicardianContract, pb.OrderState, bool, []*spvwallet.TransactionRecord, bool, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	stmt, err := p.db.Prepare("select contract, state, funded, transactions, read from purchases where orderID=?")
	defer stmt.Close()
	var contract []byte
	var stateInt int
	var fundedInt *int
	var readInt *int
	var serializedTransactions []byte
	err = stmt.QueryRow(orderId).Scan(&contract, &stateInt, &fundedInt, &serializedTransactions, &readInt)
	if err != nil {
		return nil, pb.OrderState(0), false, nil, false, err
	}
	rc := new(pb.RicardianContract)
	err = jsonpb.UnmarshalString(string(contract), rc)
	if err != nil {
		return nil, pb.OrderState(0), false, nil, false, err
	}
	funded := false
	if fundedInt != nil && *fundedInt == 1 {
		funded = true
	}
	read := false
	if readInt != nil && *readInt == 1 {
		read = true
	}
	var records []*spvwallet.TransactionRecord
	json.Unmarshal(serializedTransactions, &records)
	return rc, pb.OrderState(stateInt), funded, records, read, nil
}

func (p *PurchasesDB) Count() int {
	p.lock.RLock()
	defer p.lock.RUnlock()
	row := p.db.QueryRow("select Count(*) from purchases")
	var count int
	row.Scan(&count)
	return count
}

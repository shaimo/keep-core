const crypto = require("crypto")
const KeepRandomBeacon = artifacts.require("KeepRandomBeaconImplV1")
const KeepRandomBeaconProxy = artifacts.require('KeepRandomBeacon.sol')

// Example usage:
// truffle exec ./scripts/request-relay-entry-with-callback.js yourContractAddress "callbackMethodName" payment
// truffle exec ./scripts/request-relay-entry-with-callback.js 0x9F57C01059057d821c6b4B04A4598322661C934F "callback(uint256)" 100

module.exports = async function() {

  const keepRandomBeaconProxy = await KeepRandomBeaconProxy.deployed()
  const contractInstance = await KeepRandomBeacon.at(keepRandomBeaconProxy.address)

  try {
    let tx = await contractInstance.methods['requestRelayEntry(uint256,address,string)'](crypto.randomBytes(32), process.argv[4], process.argv[5], {value: process.argv[6]})
    console.log('Successfully requested relay entry with a callback. RequestId =', tx.logs[0].args.requestID.toString())
    console.log(
      '\n---Transaction Summary---' + '\n' +
      'From:' + tx.receipt.from + '\n' +
      'To:' + tx.receipt.to + '\n' +
      'BlockNumber:' + tx.receipt.blockNumber + '\n' +
      'TotalGas:' + tx.receipt.cumulativeGasUsed + '\n' +
      'TransactionHash:' + tx.receipt.transactionHash + '\n' +
      '--------------------------'
    )
  } catch(error) {
    console.error('Request failed with', error)
  }

  process.exit()
}
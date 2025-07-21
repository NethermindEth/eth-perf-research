# Ethereum Performance Benchmarking

This repository hosts infrastructure, tooling, and research focused on benchmarking Ethereum client performance under varying gas limits and state sizes. The goal is to enable safe, data-driven protocol tuning and provide actionable insights for the Ethereum ecosystem.

## 🌐 Project Scope

- **Gas Limit Analysis**  
  Evaluate Ethereum mainnet performance under varying gas limits. Recommend a safe, optimized gas limit based on real data and a public checklist.

- **State Bloat Research**  
  Assess the performance impact of large state sizes (e.g. 10× mainnet) across clients and scenarios. Establish whether state growth meaningfully affects throughput and block processing.

- **Infrastructure & Coordination**  
  - Maintain advanced Perfnets for multiple purposes (stress testing, mainnet-like verification, state bloat perfnet)
  - Provide reproducible benchmarking environments  
  - Ensure coordination across Execution and Consensus Layer teams  
  - Public dashboards and transparent metrics

## 📊 Expected Outcomes

- Safe gas limit recommendation for mainnet adoption  
- Publicly available benchmarks and performance data  
- Clear checklist for evaluating protocol changes  
- Streamlined collaboration across clients  
- Transparent and trackable process for EL/CL teams

## 📈 Current Results/Findings

### Gas Limit blockers
#### **45 MGas**
- [X] OpCodes benchmarking tool having proper execution on performance branches giving enough insgihts for client teams
- [X] Besu Improve EC Scenarios
- [X] General optimizations around ModExp to reach at least 10/12 MGas/s in each EL
- [X] Ensure there is no critical issues on ZKEVM tests
#### **60 MGas**
- [ ] Further Modexp Improvements for Geth and Erigon
- [X] Point Precompile analysis
- [ ] XEN and heavy contracts testing + automation on perfnet
#### **85 Mgas**
- [ ] Receipts size too big - to be addressed in https://eips.ethereum.org/EIPS/eip-7975
#### **100 MGas**
- [ ] Point Precomiple repricing - EIP to be created
#### **105 MGas**
- [ ] Calldata further repricing?

### State Bloating
- [X] Creating initial infrastructure for State Bloating
- [X] Validating various options for state bloat - Geth/Nethermind plugins or Manual pruning via State-heavy transactions
- [X] Creating first scenarios to bloat a state via Spamoor
- [X] First 50GB bloated
- [X] 1,5X Mainnet Size
- [ ] 2X Mainnet Size
- [ ] SyncTests executed on 2X state as a baseline comparing to 1X
- [ ] 3X Mainnet Size
- [ ] 5X Mainnet Size
- [ ] 10X Mainnet Size

### Various issues reported or fixes made thanks to findings during testing
- [Client_name] Description + Issue Link/PR Link

## 🧠 Who Is This For?

- Ethereum client developers (EL and CL)  
- Core protocol researchers  
- Infra engineers and performance analysts  
- Ethereum community members interested in network scalability

## 🤝 Contributions

Contributions are welcome! Please open issues for bugs, ideas, or areas that need clarification.  
For major changes or proposals, open a discussion first.

## 📌 License

MIT License

const { ethers, upgrades } = require('hardhat');
const os = require('os');
const fs = require('fs');
const path = require('path');
const pathOutputJson = path.join(os.homedir(), './iden3_deploy_output.json');

async function main() {
  const Verifier = await ethers.getContractFactory('Verifier');

  console.log('deploying verifier');
  const verifier = await Verifier.deploy();
  await verifier.deployed();

  console.log('deploying state');
  const State = await ethers.getContractFactory('State');
  const state = await upgrades.deployProxy(State, [verifier.address]);
  await state.deployed();

  console.log(`Verifier contract deployed to ${verifier.address} from ${(await ethers.getSigners())[0].address}`);
  console.log(`State contract deployed to ${state.address} from ${(await ethers.getSigners())[0].address}`);

  const outputJson = {
    state: state.address,
    verifier: verifier.address,
    network: process.env.HARDHAT_NETWORK,
  };
  fs.writeFileSync(pathOutputJson, JSON.stringify(outputJson, null, 1));
}

main()
  .then(() => process.exit(0))
  .catch((error) => {
    console.error(error);
    process.exit(1);
  });

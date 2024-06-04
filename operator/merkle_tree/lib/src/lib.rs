use lambdaworks_crypto::merkle_tree::merkle::MerkleTree;
use batcher::types::{VerificationCommitmentBatch, VerificationData};

const MAX_BATCH_SIZE: usize = 2 * 1024 * 1024 * 10;

#[no_mangle]
pub extern "C" fn verify_merkle_tree_batch_ffi(
    batch_bytes: &[u8; MAX_BATCH_SIZE],
    batch_len: u32,
    merkle_root: &[u8; 32]
) -> bool {
    match serde_json::from_slice::<Vec<VerificationData>>(&batch_bytes[..batch_len as usize]) {
        Ok(batch) => {
            let batch_commitment = VerificationCommitmentBatch::from(&batch);
            let batch_merkle_tree: MerkleTree<VerificationCommitmentBatch> = MerkleTree::build(&batch_commitment.0);
            let batch_merkle_root = hex::encode(batch_merkle_tree.root); 

            let received_merkle_root = hex::encode(merkle_root);
            
            println!("Calculated Merkle Root: {}", batch_merkle_root);
            println!("Received Merkle Root: {}", received_merkle_root);

            batch_merkle_root == received_merkle_root
        },
        Err(_) => {
            eprintln!("Failed to parse batch data");
            false
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::File;
    use std::io::Read;

    #[test]
    fn test_verify_merkle_tree_batch_ffi() {
        let path = "./test_files/7a3d9215cfac21a4b0e94382e53a9f26bc23ed990f9c850a31ccf3a65aec1466.json";

        let mut file = File::open(path).unwrap();

        let mut bytes_vec = Vec::new();
        file.read_to_end(&mut bytes_vec).unwrap();

        let mut bytes = [0; MAX_BATCH_SIZE];
        bytes[..bytes_vec.len()].copy_from_slice(&bytes_vec);

        let mut merkle_root = [0; 32];
        merkle_root.copy_from_slice(&hex::decode("7a3d9215cfac21a4b0e94382e53a9f26bc23ed990f9c850a31ccf3a65aec1466").unwrap());

        let result = verify_merkle_tree_batch_ffi(&bytes, bytes_vec.len() as u32, &merkle_root);

        assert_eq!(result, true);
    }
    

    #[test]
    fn test_verify_merkle_tree_batch_ffi_bad_proof() {
        let path = "./test_files/copy.json";
        let mut file = File::open(path).unwrap();
        let mut bytes_vec = Vec::new();
        file.read_to_end(&mut bytes_vec).unwrap();

        let mut bytes = [0; MAX_BATCH_SIZE];
        bytes[..bytes_vec.len()].copy_from_slice(&bytes_vec);
        bytes[0] = bytes[0] ^ 0x01; // Flip a bit

        let mut merkle_root = [0; 32];
        merkle_root.copy_from_slice(&hex::decode("7a3d9215cfac21a4b0e94382e53a9f26bc23ed990f9c850a31ccf3a65aec1466").unwrap());

        let result = verify_merkle_tree_batch_ffi(&bytes, bytes_vec.len() as u32, &merkle_root);
        assert!(!result);
    }
}
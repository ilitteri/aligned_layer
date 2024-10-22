use backon::ExponentialBuilder;
use backon::Retryable;
use ethers::prelude::*;
use futures_util::{stream::SplitSink, SinkExt};
use log::warn;
use std::sync::Arc;
use std::{future::Future, time::Duration};
use tokio::net::TcpStream;
use tokio::sync::RwLock;
use tokio_tungstenite::tungstenite;
use tokio_tungstenite::WebSocketStream;

use crate::eth::payment_service::BatcherPaymentService;

pub const DEFAULT_MIN_DELAY: u64 = 2000;
pub const DEFAULT_MAX_TIMES: usize = 3;
pub const DEFAULT_FACTOR: f32 = 2.0;

#[derive(Debug)]
pub enum RetryError<E> {
    Transient(E),
    Permanent(E),
}

impl<E: std::fmt::Display> std::fmt::Display for RetryError<E> {
    fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
        match self {
            RetryError::Transient(e) => write!(f, "{}", e),
            RetryError::Permanent(e) => write!(f, "{}", e),
        }
    }
}

impl<E> RetryError<E> {
    pub fn inner(self) -> E {
        match self {
            RetryError::Transient(e) => e,
            RetryError::Permanent(e) => e,
        }
    }
}

impl<E: std::fmt::Display> std::error::Error for RetryError<E> where E: std::fmt::Debug {}

pub async fn retry_function<FutureFn, Fut, T, E>(
    function: FutureFn,
    min_delay: u64,
    factor: f32,
    max_times: usize,
) -> Result<T, RetryError<E>>
where
    Fut: Future<Output = Result<T, RetryError<E>>>,
    FutureFn: FnMut() -> Fut,
{
    let backoff = ExponentialBuilder::default()
        .with_min_delay(Duration::from_millis(min_delay))
        .with_max_times(max_times)
        .with_factor(factor);

    function
        .retry(backoff)
        .sleep(tokio::time::sleep)
        .when(|e| matches!(e, RetryError::Transient(_)))
        .await
}

pub async fn get_user_balance_retryable(
    payment_service: &BatcherPaymentService,
    payment_service_fallback: &BatcherPaymentService,
    addr: &Address,
) -> Result<U256, RetryError<String>> {
    if let Ok(balance) = payment_service.user_balances(*addr).call().await {
        return Ok(balance);
    };

    payment_service_fallback
        .user_balances(*addr)
        .call()
        .await
        .map_err(|e| {
            warn!("Failed to get balance for address {:?}. Error: {e}", addr);
            RetryError::Transient(e.to_string())
        })
}

pub async fn get_user_nonce_from_ethereum_retryable(
    payment_service: &BatcherPaymentService,
    payment_service_fallback: &BatcherPaymentService,
    addr: Address,
) -> Result<U256, RetryError<String>> {
    if let Ok(nonce) = payment_service.user_nonces(addr).call().await {
        return Ok(nonce);
    }
    payment_service_fallback
        .user_nonces(addr)
        .call()
        .await
        .map_err(|e| {
            warn!("Error getting user nonce: {e}");
            RetryError::Transient(e.to_string())
        })
}

pub async fn user_balance_is_unlocked_retryable(
    payment_service: &BatcherPaymentService,
    payment_service_fallback: &BatcherPaymentService,
    addr: &Address,
) -> Result<bool, RetryError<()>> {
    if let Ok(unlock_block) = payment_service.user_unlock_block(*addr).call().await {
        return Ok(unlock_block != U256::zero());
    }
    if let Ok(unlock_block) = payment_service_fallback
        .user_unlock_block(*addr)
        .call()
        .await
    {
        return Ok(unlock_block != U256::zero());
    }
    warn!("Failed to get user locking state {:?}", addr);
    Err(RetryError::Transient(()))
}

pub async fn get_gas_price_retryable(
    eth_ws_provider: &Provider<Http>,
    eth_ws_provider_fallback: &Provider<Http>,
) -> Result<U256, RetryError<String>> {
    if let Ok(gas_price) = eth_ws_provider
        .get_gas_price()
        .await
        .inspect_err(|e| warn!("Failed to get gas price. Trying with fallback: {e:?}"))
    {
        return Ok(gas_price);
    }

    eth_ws_provider_fallback.get_gas_price().await.map_err(|e| {
        warn!("Failed to get fallback gas price: {e:?}");
        RetryError::Transient(e.to_string())
    })
}

pub async fn send_response_retryable(
    ws_sink: &Arc<RwLock<SplitSink<WebSocketStream<TcpStream>, tungstenite::Message>>>,
    serialized_response: Vec<u8>,
) -> Result<(), RetryError<tungstenite::Error>> {
    let sending_result = ws_sink
        .write()
        .await
        .send(tungstenite::Message::binary(serialized_response))
        .await;

    match sending_result {
        Err(tungstenite::Error::AlreadyClosed) => {
            Err(RetryError::Permanent(tungstenite::Error::AlreadyClosed))
        }
        Err(e) => Err(RetryError::Transient(e)),
        Ok(_) => Ok(()),
    }
}

#[cfg(test)]
mod test {
    use super::*;
    use crate::{
        config::ECDSAConfig,
        eth::{self, get_provider, payment_service::BatcherPaymentService},
    };
    use ethers::{
        contract::abigen,
        types::{Address, U256},
        utils::{Anvil, AnvilInstance},
    };
    use futures_util::StreamExt;
    use std::{str::FromStr, sync::Arc};
    use tokio::{
        net::{TcpListener, TcpStream},
        sync::RwLock,
    };

    abigen!(
        BatcherPaymentServiceContract,
        "../aligned-sdk/abi/BatcherPaymentService.json"
    );

    async fn setup_anvil(port: u16) -> (AnvilInstance, BatcherPaymentService) {
        let anvil = Anvil::new()
            .port(port)
            .arg("--load-state")
            .arg("../../contracts/scripts/anvil/state/alignedlayer-deployed-anvil-state.json")
            .spawn();

        let eth_rpc_provider = eth::get_provider(format!("http://localhost:{}", port))
            .expect("Failed to get provider");

        let payment_service_addr = String::from("0x7969c5eD335650692Bc04293B07F5BF2e7A673C0");

        let payment_service = eth::payment_service::get_batcher_payment_service(
            eth_rpc_provider,
            ECDSAConfig {
                private_key_store_path: "../../config-files/anvil.batcher.ecdsa.key.json"
                    .to_string(),
                private_key_store_password: "".to_string(),
            },
            payment_service_addr,
        )
        .await
        .expect("Failed to get Batcher Payment Service contract");
        (anvil, payment_service)
    }

    #[tokio::test]
    async fn test_get_user_balance_retryable() {
        let payment_service;
        let dummy_user_addr =
            Address::from_str("0x8969c5eD335650692Bc04293B07F5BF2e7A673C0").unwrap();
        {
            let _anvil;
            (_anvil, payment_service) = setup_anvil(8545u16).await;

            let balance =
                get_user_balance_retryable(&payment_service, &payment_service, &dummy_user_addr)
                    .await
                    .unwrap();

            assert_eq!(balance, U256::zero());
            // Kill anvil
        }

        let result =
            get_user_balance_retryable(&payment_service, &payment_service, &dummy_user_addr).await;
        assert!(matches!(result, Err(RetryError::Transient(_))));

        // restart anvil
        let (_anvil, _) = setup_anvil(8545u16).await;
        let balance =
            get_user_balance_retryable(&payment_service, &payment_service, &dummy_user_addr)
                .await
                .unwrap();

        assert_eq!(balance, U256::zero());
    }

    #[tokio::test]
    async fn test_user_balance_is_unlocked_retryable() {
        let payment_service;
        let dummy_user_addr =
            Address::from_str("0x8969c5eD335650692Bc04293B07F5BF2e7A673C0").unwrap();

        {
            let _anvil;
            (_anvil, payment_service) = setup_anvil(8546u16).await;
            let unlocked = user_balance_is_unlocked_retryable(
                &payment_service,
                &payment_service,
                &dummy_user_addr,
            )
            .await
            .unwrap();

            assert_eq!(unlocked, false);
            // Kill Anvil
        }

        let result = user_balance_is_unlocked_retryable(
            &payment_service,
            &payment_service,
            &dummy_user_addr,
        )
        .await;
        assert!(matches!(result, Err(RetryError::Transient(_))));

        // restart Anvil
        let (_anvil, payment_service) = setup_anvil(8546u16).await;
        let unlocked = user_balance_is_unlocked_retryable(
            &payment_service,
            &payment_service,
            &dummy_user_addr,
        )
        .await
        .unwrap();

        assert_eq!(unlocked, false);
    }

    #[tokio::test]
    async fn test_get_user_nonce_retryable() {
        let payment_service;
        let dummy_user_addr =
            Address::from_str("0x8969c5eD335650692Bc04293B07F5BF2e7A673C0").unwrap();
        {
            let _anvil;
            (_anvil, payment_service) = setup_anvil(8547u16).await;
            let nonce = get_user_nonce_from_ethereum_retryable(
                &payment_service,
                &payment_service,
                dummy_user_addr,
            )
            .await
            .unwrap();

            assert_eq!(nonce, U256::zero());
            // Kill Anvil
        }

        let result = get_user_nonce_from_ethereum_retryable(
            &payment_service,
            &payment_service,
            dummy_user_addr,
        )
        .await;
        assert!(matches!(result, Err(RetryError::Transient(_))));

        // restart Anvil
        let (_anvil, payment_service) = setup_anvil(8547u16).await;

        let nonce = get_user_nonce_from_ethereum_retryable(
            &payment_service,
            &payment_service,
            dummy_user_addr,
        )
        .await
        .unwrap();

        assert_eq!(nonce, U256::zero());
    }

    #[tokio::test]
    async fn test_get_gas_price_retryable() {
        let eth_rpc_provider;
        {
            let (_anvil, _payment_service) = setup_anvil(8548u16).await;
            eth_rpc_provider = get_provider("http://localhost:8548".to_string())
                .expect("Failed to get ethereum websocket provider");
            let result = get_gas_price_retryable(&eth_rpc_provider, &eth_rpc_provider).await;

            assert!(result.is_ok());
            // kill Anvil
        }
        let result = get_gas_price_retryable(&eth_rpc_provider, &eth_rpc_provider).await;
        assert!(matches!(result, Err(RetryError::Transient(_))));

        // restart Anvil
        let (_anvil, _payment_service) = setup_anvil(8548u16).await;
        let result = get_gas_price_retryable(&eth_rpc_provider, &eth_rpc_provider).await;

        assert!(result.is_ok());
    }

    #[tokio::test]
    async fn test_send_response_retryable() {
        let listener = TcpListener::bind("localhost:8553").await.unwrap();

        let client_handle = tokio::spawn(async move {
            let stream = TcpStream::connect("localhost:8553")
                .await
                .expect("Failed to connect");

            let (mut ws_stream, _) = tokio_tungstenite::client_async("ws://localhost:8553", stream)
                .await
                .expect("WebSocket handshake failed");

            // Read the response from the server
            if let None = ws_stream.next().await {
                panic!("Failed to receive valid WebSocket response");
            }
        });

        let (raw_stream, _) = listener
            .accept()
            .await
            .expect("Failed to accept connection");
        let ws_stream = tokio_tungstenite::accept_async(raw_stream).await.unwrap();
        let (outgoing, _incoming) = ws_stream.split();
        let outgoing = Arc::new(RwLock::new(outgoing));
        let message = "Some message".to_string();

        let result = send_response_retryable(&outgoing, message.clone().into_bytes()).await;
        assert!(result.is_ok());
        client_handle.await.unwrap()
    }
}
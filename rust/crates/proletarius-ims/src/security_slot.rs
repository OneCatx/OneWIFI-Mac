//! Single-owner lifecycle for the negotiated IMS security association.
//!
//! The network task owns this value directly. Coordinators may carry only the
//! move-only installation receipt/generation through their command protocol;
//! keys and packet transforms never leave the packet owner.
use crate::{
    registration_connection::{InstalledSecAgree, ReadySecAgree},
    security_policy::{SecurityBinding, SecurityPolicy},
    security_transport::{
        InboundDropReason, SecurityTransport, TransportDisposition, TransportError, packet_metadata,
    },
};
use std::{net::IpAddr, num::NonZeroU64};
use zeroize::Zeroizing;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SecurityGeneration(NonZeroU64);

impl SecurityGeneration {
    pub const fn get(self) -> u64 {
        self.0.get()
    }
}

/// Proof that one policy was installed by the packet owner. Deliberately not
/// Clone/Debug/Serialize: observing a Security-Server offer is not proof.
pub struct InstallationReceipt {
    generation: SecurityGeneration,
    binding: SecurityBinding,
}

impl InstallationReceipt {
    pub fn generation(&self) -> SecurityGeneration {
        self.generation
    }

    pub fn binding(&self) -> SecurityBinding {
        self.binding
    }
}

#[derive(Debug, PartialEq, Eq)]
pub enum SecuritySlotError {
    Occupied(SecurityGeneration),
    Empty,
    StaleGeneration {
        installed: SecurityGeneration,
        requested: SecurityGeneration,
    },
    GenerationExhausted,
    BindingInvariant,
    Transport(TransportError),
}

impl From<TransportError> for SecuritySlotError {
    fn from(error: TransportError) -> Self {
        Self::Transport(error)
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
struct FlowSelector {
    local_port: u16,
    remote_port: u16,
    inbound_spi: u32,
}

#[derive(Clone, Copy, PartialEq, Eq)]
struct SecuritySelectors {
    local: IpAddr,
    remote: IpAddr,
    flows: [FlowSelector; 2],
}

#[derive(Clone, Copy, PartialEq, Eq)]
struct SecurityDomain {
    local: IpAddr,
    remote: IpAddr,
}

impl From<SecuritySelectors> for SecurityDomain {
    fn from(selectors: SecuritySelectors) -> Self {
        Self {
            local: selectors.local,
            remote: selectors.remote,
        }
    }
}

impl SecurityDomain {
    fn selected_outbound(&self, packet: &[u8]) -> Result<bool, TransportError> {
        let metadata = packet_metadata(packet)?;
        Ok(metadata.source == self.local
            && metadata.destination == self.remote
            && metadata.protocol == 6)
    }

    fn selected_inbound(&self, packet: &[u8]) -> Result<bool, TransportError> {
        let metadata = packet_metadata(packet)?;
        Ok(metadata.source == self.remote
            && metadata.destination == self.local
            && matches!(metadata.protocol, 6 | 50))
    }
}

impl SecuritySelectors {
    fn from_policy(policy: &SecurityPolicy) -> Self {
        Self {
            local: policy.local_ip,
            remote: policy.remote_ip,
            flows: [
                FlowSelector {
                    local_port: policy.client_flow.local_port,
                    remote_port: policy.client_flow.remote_port,
                    inbound_spi: policy.client_flow.inbound_spi,
                },
                FlowSelector {
                    local_port: policy.server_flow.local_port,
                    remote_port: policy.server_flow.remote_port,
                    inbound_spi: policy.server_flow.inbound_spi,
                },
            ],
        }
    }

    fn selected_outbound(&self, packet: &[u8]) -> Result<bool, TransportError> {
        let metadata = packet_metadata(packet)?;
        if metadata.source != self.local
            || metadata.destination != self.remote
            || metadata.protocol != 6
        {
            return Ok(false);
        }
        let Some((source, destination)) = metadata.tcp_ports else {
            return Err(TransportError::Packet);
        };
        Ok(self
            .flows
            .iter()
            .any(|flow| flow.local_port == source && flow.remote_port == destination))
    }

    fn selected_inbound(&self, packet: &[u8]) -> Result<bool, TransportError> {
        let metadata = packet_metadata(packet)?;
        if metadata.source != self.remote || metadata.destination != self.local {
            return Ok(false);
        }
        match metadata.protocol {
            6 => {
                let Some((source, destination)) = metadata.tcp_ports else {
                    return Err(TransportError::Packet);
                };
                Ok(self
                    .flows
                    .iter()
                    .any(|flow| flow.remote_port == source && flow.local_port == destination))
            }
            50 => Ok(metadata
                .esp_spi
                .is_some_and(|spi| self.flows.iter().any(|flow| flow.inbound_spi == spi))),
            _ => Ok(false),
        }
    }
}

struct InstalledSecurity {
    generation: SecurityGeneration,
    selectors: SecuritySelectors,
    transport: SecurityTransport,
}

/// Owned by exactly one network runtime. Installation is intentionally
/// refused while occupied: a session replacement must first close its sockets
/// and clear the exact generation, avoiding simultaneous old/new SA owners.
pub struct SecuritySlot {
    next_generation: u64,
    installed: Option<InstalledSecurity>,
    retired: Vec<SecuritySelectors>,
    retired_domains: Vec<SecurityDomain>,
    block_all_retired_tcp: bool,
}

const MAX_RETIRED_SELECTORS: usize = 16;
const MAX_RETIRED_DOMAINS: usize = 8;

impl Default for SecuritySlot {
    fn default() -> Self {
        Self::new()
    }
}

impl SecuritySlot {
    pub const fn new() -> Self {
        Self {
            next_generation: 1,
            installed: None,
            retired: Vec::new(),
            retired_domains: Vec::new(),
            block_all_retired_tcp: false,
        }
    }

    pub fn installed_generation(&self) -> Option<SecurityGeneration> {
        self.installed.as_ref().map(|active| active.generation)
    }

    pub fn install(
        &mut self,
        policy: SecurityPolicy,
    ) -> Result<InstallationReceipt, SecuritySlotError> {
        if let Some(active) = &self.installed {
            return Err(SecuritySlotError::Occupied(active.generation));
        }
        let generation = SecurityGeneration(
            NonZeroU64::new(self.next_generation).ok_or(SecuritySlotError::GenerationExhausted)?,
        );
        let next = self
            .next_generation
            .checked_add(1)
            .ok_or(SecuritySlotError::GenerationExhausted)?;
        let binding = policy.binding();
        let selectors = SecuritySelectors::from_policy(&policy);
        let transport = SecurityTransport::new(policy)?;
        self.installed = Some(InstalledSecurity {
            generation,
            selectors,
            transport,
        });
        self.next_generation = next;
        Ok(InstallationReceipt {
            generation,
            binding,
        })
    }

    /// Atomically consume a complete REGISTER transition in the packet owner.
    /// Policy and continuation are compared before installation and return as
    /// one opaque proof, so downstream code cannot pair different AKA runs.
    pub fn install_registration(
        &mut self,
        ready: ReadySecAgree,
    ) -> Result<InstalledSecAgree, SecuritySlotError> {
        let (policy, continuation) = ready.into_install_parts();
        if policy.binding() != continuation.binding() {
            return Err(SecuritySlotError::BindingInvariant);
        }
        let receipt = self.install(policy)?;
        Ok(InstalledSecAgree::new(continuation, receipt))
    }

    /// Clears only the exact active generation. A delayed cleanup command can
    /// never remove a newer SA. Retired selectors remain blocked until the
    /// whole tunnel runtime is dropped, preventing a stale socket from
    /// silently falling back to plaintext.
    pub fn clear_if(&mut self, generation: SecurityGeneration) -> Result<(), SecuritySlotError> {
        let Some(active) = self.installed.as_ref() else {
            return Err(SecuritySlotError::Empty);
        };
        if active.generation != generation {
            return Err(SecuritySlotError::StaleGeneration {
                installed: active.generation,
                requested: generation,
            });
        }
        let active = self.installed.take().expect("checked above");
        self.retire(active.selectors);
        Ok(())
    }

    fn retire(&mut self, selectors: SecuritySelectors) {
        if self.block_all_retired_tcp
            || self.retired.contains(&selectors)
            || self
                .retired_domains
                .contains(&SecurityDomain::from(selectors))
        {
            return;
        }
        if self.retired.len() < MAX_RETIRED_SELECTORS {
            self.retired.push(selectors);
            return;
        }

        // Never evict an exact tombstone back to plaintext. Once the exact
        // set is full, coarsen all retired selectors for this P-CSCF address
        // pair into a fail-closed domain tombstone. At the second bound, block
        // every non-active TCP/ESP flow in the tunnel rather than grow without
        // limit or weaken protection.
        let domain = SecurityDomain::from(selectors);
        if self.retired_domains.len() < MAX_RETIRED_DOMAINS {
            self.retired
                .retain(|item| SecurityDomain::from(*item) != domain);
            self.retired_domains.push(domain);
        } else {
            self.retired.clear();
            self.retired_domains.clear();
            self.block_all_retired_tcp = true;
        }
    }

    fn globally_retired_outbound(packet: &[u8]) -> Result<bool, TransportError> {
        Ok(packet_metadata(packet)?.protocol == 6)
    }

    fn globally_retired_inbound(packet: &[u8]) -> Result<bool, TransportError> {
        Ok(matches!(packet_metadata(packet)?.protocol, 6 | 50))
    }

    pub fn route_outbound(
        &mut self,
        packet: Zeroizing<Vec<u8>>,
    ) -> Result<TransportDisposition, SecuritySlotError> {
        let packet = if let Some(active) = &mut self.installed {
            match active.transport.route_outbound(packet)? {
                TransportDisposition::Passthrough(packet) => packet,
                disposition => return Ok(disposition),
            }
        } else {
            packet
        };
        if self.block_all_retired_tcp && Self::globally_retired_outbound(&packet).unwrap_or(true)
            || self
                .retired_domains
                .iter()
                .any(|domain| domain.selected_outbound(&packet).unwrap_or(true))
            || self
                .retired
                .iter()
                .any(|selectors| selectors.selected_outbound(&packet).unwrap_or(true))
        {
            return Ok(TransportDisposition::Drop(
                InboundDropReason::RetiredSecurityAssociation,
            ));
        }
        Ok(TransportDisposition::Passthrough(packet))
    }

    pub fn route_inbound(
        &mut self,
        packet: Zeroizing<Vec<u8>>,
    ) -> Result<TransportDisposition, SecuritySlotError> {
        let packet = if let Some(active) = &mut self.installed {
            match active.transport.route_inbound(packet)? {
                TransportDisposition::Passthrough(packet) => packet,
                disposition => return Ok(disposition),
            }
        } else {
            packet
        };
        if self.block_all_retired_tcp && Self::globally_retired_inbound(&packet).unwrap_or(true)
            || self
                .retired_domains
                .iter()
                .any(|domain| domain.selected_inbound(&packet).unwrap_or(true))
            || self
                .retired
                .iter()
                .any(|selectors| selectors.selected_inbound(&packet).unwrap_or(true))
        {
            return Ok(TransportDisposition::Drop(
                InboundDropReason::RetiredSecurityAssociation,
            ));
        }
        Ok(TransportDisposition::Passthrough(packet))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        aka_exchange::SecurityKeys, security_offer::parse_security_server,
        security_policy::LocalEndpoints, security_transport::build_ip_packet,
    };

    fn policy_for(remote_ip: &str, local_port: u16, spi_offset: u32) -> SecurityPolicy {
        let offer = parse_security_server(&format!(
            "ipsec-3gpp;alg=hmac-sha-1-96;ealg=aes-cbc;spi-c={};spi-s={};port-c=6050;port-s=6060",
            111 + spi_offset,
            222 + spi_offset
        ))
        .unwrap()
        .remove(0);
        SecurityPolicy::aes_sha1(
            "10.0.0.2".parse().unwrap(),
            remote_ip.parse().unwrap(),
            LocalEndpoints {
                spi_c: 333 + spi_offset,
                spi_s: 444 + spi_offset,
                port_c: local_port,
                port_s: local_port + 1,
            },
            &offer,
            SecurityKeys {
                ck: Zeroizing::new([1; 16]),
                ik: Zeroizing::new([2; 16]),
            },
        )
        .unwrap()
    }

    fn policy(local_port: u16, spi_offset: u32) -> SecurityPolicy {
        policy_for("10.0.0.9", local_port, spi_offset)
    }

    fn tcp_packet(
        source: &str,
        destination: &str,
        source_port: u16,
        destination_port: u16,
    ) -> Zeroizing<Vec<u8>> {
        let mut tcp = vec![0; 20];
        tcp[..2].copy_from_slice(&source_port.to_be_bytes());
        tcp[2..4].copy_from_slice(&destination_port.to_be_bytes());
        tcp[12] = 0x50;
        build_ip_packet(
            source.parse().unwrap(),
            destination.parse().unwrap(),
            6,
            &tcp,
        )
        .unwrap()
    }

    #[test]
    fn install_clear_generation_is_fail_closed_without_plaintext_fallback() {
        let mut slot = SecuritySlot::new();
        let selected = tcp_packet("10.0.0.2", "10.0.0.9", 5064, 6060);
        assert!(matches!(
            slot.route_outbound(selected.clone()).unwrap(),
            TransportDisposition::Passthrough(_)
        ));

        let first = slot.install(policy(5064, 0)).unwrap();
        let first_generation = first.generation();
        assert_eq!(slot.installed_generation(), Some(first_generation));
        assert!(matches!(
            slot.route_outbound(selected.clone()).unwrap(),
            TransportDisposition::Protected(_)
        ));
        assert!(matches!(
            slot.install(policy(5074, 1000)),
            Err(SecuritySlotError::Occupied(generation)) if generation == first_generation
        ));

        slot.clear_if(first_generation).unwrap();
        assert_eq!(slot.installed_generation(), None);
        assert!(matches!(
            slot.route_outbound(selected.clone()).unwrap(),
            TransportDisposition::Drop(InboundDropReason::RetiredSecurityAssociation)
        ));

        let second = slot.install(policy(5074, 1000)).unwrap();
        let second_generation = second.generation();
        assert_ne!(second_generation, first_generation);
        assert!(matches!(
            slot.clear_if(first_generation),
            Err(SecuritySlotError::StaleGeneration { installed, requested })
                if installed == second_generation && requested == first_generation
        ));
        assert_eq!(slot.installed_generation(), Some(second_generation));
        assert!(matches!(
            slot.route_outbound(tcp_packet("10.0.0.2", "10.0.0.9", 5074, 6060))
                .unwrap(),
            TransportDisposition::Protected(_)
        ));
        // The old exact selector remains blocked while unrelated traffic is
        // untouched by the IMS security slot.
        assert!(matches!(
            slot.route_outbound(selected).unwrap(),
            TransportDisposition::Drop(InboundDropReason::RetiredSecurityAssociation)
        ));
        assert!(matches!(
            slot.route_outbound(tcp_packet("10.0.0.2", "10.0.0.9", 40000, 443))
                .unwrap(),
            TransportDisposition::Passthrough(_)
        ));
    }

    #[test]
    fn repeated_reauthentication_coarsens_tombstones_without_plaintext_fallback() {
        let mut slot = SecuritySlot::new();
        for attempt in 0..100_u16 {
            let receipt = slot
                .install(policy(5_000 + attempt * 2, u32::from(attempt) * 1_000))
                .unwrap();
            slot.clear_if(receipt.generation()).unwrap();
        }

        assert!(slot.retired.len() <= MAX_RETIRED_SELECTORS);
        assert_eq!(slot.retired_domains.len(), 1);
        assert!(!slot.block_all_retired_tcp);
        assert!(matches!(
            slot.route_outbound(tcp_packet("10.0.0.2", "10.0.0.9", 5_000, 6_060))
                .unwrap(),
            TransportDisposition::Drop(InboundDropReason::RetiredSecurityAssociation)
        ));
        // Coarsening is deliberately fail-closed for other TCP flows to the
        // same P-CSCF; it never evicts an old selector back to plaintext.
        assert!(matches!(
            slot.route_outbound(tcp_packet("10.0.0.2", "10.0.0.9", 40_000, 443))
                .unwrap(),
            TransportDisposition::Drop(InboundDropReason::RetiredSecurityAssociation)
        ));

        let active = slot.install(policy(6_000, 200_000)).unwrap();
        assert!(matches!(
            slot.route_outbound(tcp_packet("10.0.0.2", "10.0.0.9", 6_000, 6_060))
                .unwrap(),
            TransportDisposition::Protected(_)
        ));
        slot.clear_if(active.generation()).unwrap();
    }

    #[test]
    fn distinct_domain_churn_stays_bounded_and_active_policy_wins_global_block() {
        let mut slot = SecuritySlot::new();
        for domain in 0..=MAX_RETIRED_DOMAINS {
            let remote = format!("10.1.{domain}.1");
            for selector in 0..=MAX_RETIRED_SELECTORS {
                let serial = (domain * 100 + selector) as u32;
                let receipt = slot
                    .install(policy_for(&remote, 10_000 + selector as u16, serial))
                    .unwrap();
                slot.clear_if(receipt.generation()).unwrap();
            }
        }

        assert!(slot.block_all_retired_tcp);
        assert!(slot.retired.is_empty());
        assert!(slot.retired_domains.is_empty());
        assert!(matches!(
            slot.route_outbound(tcp_packet("10.0.0.2", "192.0.2.9", 40_000, 443))
                .unwrap(),
            TransportDisposition::Drop(InboundDropReason::RetiredSecurityAssociation)
        ));

        let active = slot
            .install(policy_for("10.9.9.9", 20_000, 50_000))
            .unwrap();
        assert!(matches!(
            slot.route_outbound(tcp_packet("10.0.0.2", "10.9.9.9", 20_000, 6_060))
                .unwrap(),
            TransportDisposition::Protected(_)
        ));
        slot.clear_if(active.generation()).unwrap();
    }
}
